/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package inst

import (
	"errors"
	"fmt"
	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/math"
	"github.com/outbrain/golib/sqlutils"
	"github.com/outbrain/orchestrator/config"
	"github.com/outbrain/orchestrator/db"
	"github.com/pmylund/go-cache"
	"regexp"
	"time"
)

const binlogEventsChunkSize int = 1000000

var instancePseudoGTIDEntryCache = cache.New(time.Duration(10)*time.Minute, time.Minute)

func getInstancePseudoGTIDKey(instance *Instance, entry string) string {
	return fmt.Sprintf("%s;%s", instance.Key.DisplayString, entry)
}

// Try and find the last position of a pseudo GTID query entry in the given binary log.
// Also return the full text of that entry.
// maxCoordinates is the position beyond which we should not read. This is relevant when reading relay logs; in particular,
// the last relay log. We must be careful not to scan for Pseudo-GTID entries past the position executed by the SQL thread.
// maxCoordinates == nil means no limit.
func getLastPseudoGTIDEntryInBinlog(instanceKey *InstanceKey, binlog string, binlogType BinlogType, maxCoordinates *BinlogCoordinates) (*BinlogCoordinates, string, error) {
	binlogCoordinates := BinlogCoordinates{LogFile: binlog, LogPos: 0, Type: binlogType}
	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)
	if err != nil {
		return nil, "", err
	}

	moreRowsExpected := true
	step := 0

	entryText := ""
	commandToken := math.TernaryString(binlogCoordinates.Type == BinaryLog, "binlog", "relaylog")
	for moreRowsExpected {
		query := fmt.Sprintf("show %s events in '%s' LIMIT %d,%d", commandToken, binlog, (step * binlogEventsChunkSize), binlogEventsChunkSize)

		moreRowsExpected = false
		err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
			moreRowsExpected = true
			binlogEntryInfo := m.GetString("Info")
			if matched, _ := regexp.MatchString(config.Config.PseudoGTIDPattern, binlogEntryInfo); matched {
				if maxCoordinates != nil && maxCoordinates.SmallerThan(&BinlogCoordinates{LogFile: binlog, LogPos: m.GetInt64("Pos")}) {
					// past the limitation
					moreRowsExpected = false
					return nil
				}
				binlogCoordinates.LogPos = m.GetInt64("Pos")
				entryText = binlogEntryInfo
				// Found a match. But we keep searching: we're interested in the LAST entry, and, alas,
				// we can only search in ASCENDING order...
			}
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		step++
	}

	// Not found? return nil. an error is reserved to SQL problems.
	if binlogCoordinates.LogPos == 0 {
		return nil, "", nil
	}
	return &binlogCoordinates, entryText, err
}

func GetLastPseudoGTIDEntryInInstance(instance *Instance) (*BinlogCoordinates, string, error) {
	// Look for last GTID in instance:
	instanceBinlogs := instance.GetBinaryLogs()

	for i := len(instanceBinlogs) - 1; i >= 0; i-- {
		log.Debugf("Searching for latest pseudo gtid entry in binlog %+v of %+v", instanceBinlogs[i], instance.Key)
		resultCoordinates, entryInfo, err := getLastPseudoGTIDEntryInBinlog(&instance.Key, instanceBinlogs[i], BinaryLog, nil)
		if err != nil {
			return nil, "", err
		}
		if resultCoordinates != nil {
			log.Debugf("Found pseudo gtid entry in %+v: %+v", instance.Key, resultCoordinates)
			return resultCoordinates, entryInfo, err
		}
	}
	return nil, "", log.Errorf("Cannot find pseudo GTID entry in binlogs of %+v", instance.Key)
}

func GetLastPseudoGTIDEntryInRelayLogs(instance *Instance, recordedInstanceRelayLogCoordinates BinlogCoordinates) (*BinlogCoordinates, string, error) {
	// Look for last GTID in relay logs:
	// Since MySQL does not provide with a SHOW RELAY LOGS command, we heuristically srtart from current
	// relay log (indiciated by Relay_log_file) and walk backwards.
	// Eventually we will hit a relay log name which does not exist.
	currentRelayLog := recordedInstanceRelayLogCoordinates
	var err error = nil
	for err == nil {
		log.Debugf("Searching for latest pseudo gtid entry in relaylog %+v of %+v, up to pos %+v", currentRelayLog.LogFile, instance.Key, recordedInstanceRelayLogCoordinates)
		if resultCoordinates, entryInfo, err := getLastPseudoGTIDEntryInBinlog(&instance.Key, currentRelayLog.LogFile, RelayLog, &recordedInstanceRelayLogCoordinates); err != nil {
			return nil, "", err
		} else if resultCoordinates != nil {
			log.Debugf("Found pseudo gtid entry in %+v: %+v", instance.Key, resultCoordinates)
			return resultCoordinates, entryInfo, err
		}
		currentRelayLog, err = currentRelayLog.PreviousFileCoordinates()
	}
	return nil, "", log.Errorf("Cannot find pseudo GTID entry in relay logs of %+v", instance.Key)
}

// Given a binlog entry text (query), search it in the given binary log of a given instance
func SearchPseudoGTIDEntryInBinlog(instanceKey *InstanceKey, binlog string, entryText string) (BinlogCoordinates, error) {
	binlogCoordinates := BinlogCoordinates{LogFile: binlog, LogPos: 0, Type: BinaryLog}
	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)
	if err != nil {
		return binlogCoordinates, err
	}

	moreRowsExpected := true
	step := 0

	commandToken := math.TernaryString(binlogCoordinates.Type == BinaryLog, "binlog", "relaylog")
	for moreRowsExpected {
		query := fmt.Sprintf("show %s events in '%s' LIMIT %d,%d", commandToken, binlog, (step * binlogEventsChunkSize), binlogEventsChunkSize)
		moreRowsExpected = false
		err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
			if binlogCoordinates.LogPos != 0 {
				return nil
				// moreRowsExpected reamins false, this quits the loop
			}
			moreRowsExpected = true
			if m.GetString("Info") == entryText {
				// found it!
				binlogCoordinates.LogPos = m.GetInt64("Pos")
			}
			return nil
		})
		if err != nil {
			return binlogCoordinates, err
		}
		step++
	}

	if binlogCoordinates.LogPos == 0 {
		return binlogCoordinates, errors.New(fmt.Sprintf("Cannot match pseudo GTID entry in binlog '%s'", binlog))
	}
	return binlogCoordinates, err
}

func SearchPseudoGTIDEntryInInstance(instance *Instance, entryText string) (*BinlogCoordinates, error) {
	cacheKey := getInstancePseudoGTIDKey(instance, entryText)
	coords, found := instancePseudoGTIDEntryCache.Get(cacheKey)
	if found {
		// This is wonderful. We can skip the tedious GTID search in the binary log
		log.Debugf("Found instance Pseudo GTID entry coordinates in cache: %+v, %+v, %+v", instance.Key, entryText, coords)
		return coords.(*BinlogCoordinates), nil
	}
	// Look for GTID entry in other-instance:
	binlogs := instance.GetBinaryLogs()
	for i := len(binlogs) - 1; i >= 0; i-- {
		log.Debugf("Searching for given pseudo gtid entry in binlog %+v of %+v", binlogs[i], instance.Key)
		resultCoordinates, err := SearchPseudoGTIDEntryInBinlog(&instance.Key, binlogs[i], entryText)
		if resultCoordinates.LogPos != 0 && err == nil {
			log.Debugf("Matched entry in %+v: %+v", instance.Key, resultCoordinates)
			instancePseudoGTIDEntryCache.Set(cacheKey, &resultCoordinates, 0)
			return &resultCoordinates, nil
		}
	}
	return nil, log.Errorf("Cannot match pseudo GTID entry in binlogs of %+v", instance.Key)
}

// Read (as much as possible of) a chink of binary log events starting the given startingCoordinates
func readBinlogEventsChunk(instanceKey *InstanceKey, startingCoordinates BinlogCoordinates) ([]BinlogEvent, error) {
	events := []BinlogEvent{}
	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)
	if err != nil {
		return events, err
	}
	commandToken := math.TernaryString(startingCoordinates.Type == BinaryLog, "binlog", "relaylog")
	query := fmt.Sprintf("show %s events in '%s' FROM %d LIMIT %d", commandToken, startingCoordinates.LogFile, startingCoordinates.LogPos, binlogEventsChunkSize)
	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		binlogEvent := BinlogEvent{}
		binlogEvent.Coordinates.LogFile = m.GetString("Log_name")
		binlogEvent.Coordinates.LogPos = m.GetInt64("Pos")
		binlogEvent.Coordinates.Type = startingCoordinates.Type
		binlogEvent.NextEventPos = m.GetInt64("End_log_pos")
		binlogEvent.EventType = m.GetString("Event_type")
		binlogEvent.Info = m.GetString("Info")

		events = append(events, binlogEvent)
		return nil
	})
	return events, err
}

// Return the next chunk of binlog events; skip to next binary log file if need be; return empty result only
// if reached end of binary logs
func getNextBinlogEventsChunk(instance *Instance, startingCoordinates BinlogCoordinates) ([]BinlogEvent, error) {
	events, err := readBinlogEventsChunk(&instance.Key, startingCoordinates)
	if err != nil {
		return events, err
	}
	if len(events) > 0 {
		return events, nil
	}
	// events are empty
	if nextBinlogFile, err := instance.GetNextBinaryLog(startingCoordinates.LogFile); err == nil {
		nextCoordinates := BinlogCoordinates{LogFile: nextBinlogFile, LogPos: 0, Type: startingCoordinates.Type}
		return getNextBinlogEventsChunk(instance, nextCoordinates)
	}
	// No more log file. We return the empty array: but no error, since there is no error; we've just reached the end.
	// This behaviour is strictly expected by BinlogEventCursor
	return events, nil
}

// GetNextBinlogCoordinatesToMatch is given a twin-coordinates couple for a would-be slave (instanceKey) and another
// instance (otherKey).
// This is part of the match-below process, and is the heart of the operation: matching the binlog events starting
// the twin-coordinates (where both share the same Pseudo-GTID) until "instance" runs out of entries, hopefully
// before "other" runs out.
// If "other" runs out that means "instance" is more advanced in replication than "other", in which case we can't
// turn it into a slave of "other".
// Otherwise "instance" will point to the *next* binlog entry in "other"
func GetNextBinlogCoordinatesToMatch(instance *Instance, instanceCoordinates BinlogCoordinates, recordedInstanceRelayLogCoordinates BinlogCoordinates,
	other *Instance, otherCoordinates BinlogCoordinates) (*BinlogCoordinates, error) {

	fetchNextEvents := func(binlogCoordinates BinlogCoordinates) ([]BinlogEvent, error) {
		return getNextBinlogEventsChunk(instance, binlogCoordinates)
	}
	instanceCursor := NewBinlogEventCursor(instanceCoordinates, fetchNextEvents)

	fetchOtherNextEvents := func(binlogCoordinates BinlogCoordinates) ([]BinlogEvent, error) {
		return getNextBinlogEventsChunk(other, binlogCoordinates)
	}
	otherCursor := NewBinlogEventCursor(otherCoordinates, fetchOtherNextEvents)

	var lastConsumedEventCoordinates BinlogCoordinates
	for {
		// Exhaust binlogs/relaylogs on instance. While iterating them, also iterate the otherInstance binlogs.
		// We expect entries on both to match, sequentially, until instance's binlogs/relaylogs are exhausted.
		var instanceEventInfo string
		var otherEventInfo string
		{
			// Extract next binlog/relaylog entry from instance:
			event, err := instanceCursor.NextRealEvent()
			if err != nil {
				return nil, log.Errore(err)
			}
			if event != nil {
				lastConsumedEventCoordinates = event.Coordinates
			}

			switch instanceCoordinates.Type {
			case BinaryLog:
				if event == nil {
					// end of binary logs for instance:
					targetMatchCoordinates, err := otherCursor.NextCoordinates()
					if err != nil {
						return nil, log.Errore(err)
					}
					nextCoordinates, _ := instanceCursor.NextCoordinates()
					if !nextCoordinates.Equals(&instance.SelfBinlogCoordinates) {
						return nil, log.Errorf("Unexpected problem: instance binlog iteration did not end with current master status. Ended with: %+v, self coordinates: %+v", nextCoordinates, instance.SelfBinlogCoordinates)
					}
					log.Debugf("Reached end of binary logs for instance, at %+v. Other coordinates: %+v", nextCoordinates, targetMatchCoordinates)
					return &targetMatchCoordinates, nil
				}
			case RelayLog:
				// Argghhhh! SHOW RELAY LOG EVENTS IN '...' statement returns CRAPPY values for End_log_pos:
				// instead of returning the end log pos of the current statement in the *relay log*, it shows
				// the end log pos of the matching statement in the *master's binary log*!
				// Yes, there's logic to this. But this means the next-ccordinates are meaningless.
				// As result, in the case where we exhaust (following) the relay log, we cannot do our last
				// nice sanity test that we've indeed reached the Relay_log_pos coordinate; we are only at the
				// last statement, which is SMALLER than Relay_log_pos; and there isn't a "Rotate" entry to make
				// a place holder or anything. The log just ends and we can't be absolutely certain that the next
				// statement is indeed (futuristically) as End_log_pos.
				endOfScan := false
				if event == nil {
					// End of relay log...
					endOfScan = true
					log.Debugf("Reached end of relay log at %+v", recordedInstanceRelayLogCoordinates)
				} else if recordedInstanceRelayLogCoordinates.Equals(&event.Coordinates) {
					// We've passed the maxScanInstanceCoordinates (applies for relay logs)
					endOfScan = true
					log.Debugf("Reached slave relay log coordinates at %+v", recordedInstanceRelayLogCoordinates)
				} else if recordedInstanceRelayLogCoordinates.SmallerThan(&event.Coordinates) {
					return nil, log.Errorf("Unexpected problem: relay log scan passed relay log position without hitting it. Ended with: %+v, relay log position: %+v", event.Coordinates, recordedInstanceRelayLogCoordinates)
				}
				if endOfScan {
					// end of binary logs for instance:
					targetMatchCoordinates, err := otherCursor.NextCoordinates()
					if err != nil {
						return nil, log.Errore(err)
					}
					// No further sanity checks (read the above lengthy explanation)
					log.Debugf("Reached limit of relay logs for instance, just after %+v. Other coordinates: %+v", lastConsumedEventCoordinates, targetMatchCoordinates)
					return &targetMatchCoordinates, nil
				}
			}

			instanceEventInfo = event.Info
			log.Debugf("> %+v %+v; %+v", event.Coordinates, event.EventType, event.Info)
		}
		{
			// Extract next binlog/relaylog entry from otherInstance (intended master):
			event, err := otherCursor.NextRealEvent()
			if err != nil {
				return nil, log.Errore(err)
			}
			if event == nil {
				// end of binary logs for otherInstance: this is unexpected and means instance is more advanced
				// than otherInstance
				return nil, log.Error("Unexpected end of binary logs for assumed master. This means the instance which attempted to be a slave was more advanced. Try the other way round")
			}
			otherEventInfo = event.Info
			log.Debugf("< %+v %+v; %+v", event.Coordinates, event.EventType, event.Info)
		}
		// Verify things are sane (the two extracted entries are identical):
		// (not strictly required by the algorithm but adds such a lovely self-sanity-testing essence)
		if instanceEventInfo != otherEventInfo {
			return nil, log.Errorf("Mismatching entries, aborting: %+v <-> %+v", instanceEventInfo, otherEventInfo)
		}
	}

	return nil, log.Error("GetNextBinlogCoordinatesToMatch: unexpected termination")
}
