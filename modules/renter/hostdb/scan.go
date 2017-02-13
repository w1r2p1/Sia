package hostdb

// scan.go contains the functions which periodically scan the list of all hosts
// to see which hosts are online or offline, and to get any updates to the
// settings of the hosts.

import (
	"crypto/rand"
	"math/big"
	"net"
	"time"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
)

// queueScan will add a host to the queue to be scanned.
func (hdb *HostDB) queueScan(entry modules.HostDBEntry) {
	// If this entry is already in the scan pool, can return immediately.
	_, exists := hdb.scanMap[entry.PublicKey.String()]
	if exists {
		return
	}

	// Add the entry to a waitlist, then check if any thread is currently
	// emptying the waitlist. If not, spawn a thread to empty the waitlist.
	hdb.scanMap[entry.PublicKey.String()] = struct{}{}
	hdb.scanList = append(hdb.scanList, entry)
	if hdb.scanWait {
		// Another thread is emptying the scan list, nothing to worry about.
		return
	}

	// Sanity check - the scan map and the scan list should have the same
	// length.
	if build.DEBUG && len(hdb.scanMap) > len(hdb.scanList)+scanningThreads {
		hdb.log.Critical("The hostdb scan map has seemingly grown too large:", len(hdb.scanMap), len(hdb.scanList), scanningThreads)
	}

	hdb.scanWait = true
	go func() {
		// Nobody is emptying the scan list, volunteer.
		if hdb.tg.Add() != nil {
			// Hostdb is shutting down, don't spin up another thread.  It is
			// okay to leave scanWait set to true as that will not affect
			// shutdown.
			return
		}
		defer hdb.tg.Done()

		for {
			hdb.mu.Lock()
			if len(hdb.scanList) == 0 {
				// Scan list is empty, can exit. Let the world know that nobody
				// is emptying the scan list anymore.
				hdb.scanWait = false
				hdb.mu.Unlock()
				return
			}
			// Get the next host, shrink the scan list.
			entry := hdb.scanList[0]
			hdb.scanList = hdb.scanList[1:]
			delete(hdb.scanMap, entry.PublicKey.String())
			scansRemaining := len(hdb.scanList)

			// Grab the most recent entry for this host.
			recentEntry, exists := hdb.hostTree.Select(entry.PublicKey)
			if exists {
				entry = recentEntry
			}
			hdb.mu.Unlock()

			// Block while waiting for an opening in the scan pool.
			hdb.log.Debugf("Sending host %v for scan, %v hosts remain", entry.PublicKey.String(), scansRemaining)
			select {
			case hdb.scanPool <- entry:
				// iterate again
			case <-hdb.tg.StopChan():
				// quit
				return
			}
		}
	}()
}

// updateEntry updates an entry in the hostdb after a scan has taken place.
//
// CAUTION: This function will automatically add multiple entries to a new host
// to give that host some base uptime. This makes this function co-dependent
// with the host weight functions. Adjustment of the host weight functions need
// to keep this function in mind, and vice-versa.
func (hdb *HostDB) updateEntry(entry modules.HostDBEntry, netErr error) {
	// If the host is not online, toss out this update.
	if !hdb.online {
		return
	}

	// Grab the host from the host tree.
	newEntry, exists := hdb.hostTree.Select(entry.PublicKey)
	if exists {
		newEntry.HostExternalSettings = entry.HostExternalSettings
	} else {
		newEntry = entry
	}

	// Add the datapoints for the scan.
	if len(newEntry.ScanHistory) < 2 {
		// Add two scans to the scan history. Two are needed because the scans
		// are forward looking, but we want this first scan to represent as
		// much as one week of uptime or downtime.
		earliestStartTime := time.Now().Add(time.Hour * 7 * 24 * -1) // Permit up to a week of starting uptime or downtime.
		suggestedStartTime := time.Now().Add(time.Minute * 10 * time.Duration(hdb.blockHeight-entry.FirstSeen) * -1)
		if suggestedStartTime.Before(earliestStartTime) {
			suggestedStartTime = earliestStartTime
		}
		newEntry.ScanHistory = modules.HostDBScans{
			{Timestamp: suggestedStartTime, Success: netErr == nil},
			{Timestamp: time.Now(), Success: netErr == nil},
		}
	} else {
		if newEntry.ScanHistory[len(newEntry.ScanHistory)-1].Success && netErr != nil {
			hdb.log.Debugf("Host %v is being downgraded from an online host to an offline host: %v\n", newEntry.PublicKey.String(), netErr)
		}
		newEntry.ScanHistory = append(newEntry.ScanHistory, modules.HostDBScan{Timestamp: time.Now(), Success: netErr == nil})
	}

	// Add the updated entry
	if !exists {
		err := hdb.hostTree.Insert(newEntry)
		if err != nil {
			hdb.log.Println("ERROR: unable to insert entry which is was thought to be new:", err)
		} else {
			hdb.log.Debugf("Adding host %v to the hostdb. Net error: %v\n", newEntry.PublicKey.String(), netErr)
		}
	} else {
		err := hdb.hostTree.Modify(newEntry)
		if err != nil {
			hdb.log.Println("ERROR: unable to modify entry which is thought to exist:", err)
		} else {
			hdb.log.Debugf("Adding host %v to the hostdb. Net error: %v\n", newEntry.PublicKey.String(), netErr)
		}
	}
}

// managedScanHost will connect to a host and grab the settings, verifying
// uptime and updating to the host's preferences.
func (hdb *HostDB) managedScanHost(entry modules.HostDBEntry) {
	// Request settings from the queued host entry.
	netAddr := entry.NetAddress
	pubKey := entry.PublicKey
	hdb.log.Debugf("Scanning host %v at %v", pubKey, netAddr)

	var settings modules.HostExternalSettings
	err := func() error {
		dialer := &net.Dialer{
			Cancel:  hdb.tg.StopChan(),
			Timeout: hostRequestTimeout,
		}
		conn, err := dialer.Dial("tcp", string(netAddr))
		if err != nil {
			return err
		}
		connCloseChan := make(chan struct{})
		go func() {
			select {
			case <-hdb.tg.StopChan():
			case <-connCloseChan:
			}
			conn.Close()
		}()
		defer close(connCloseChan)
		conn.SetDeadline(time.Now().Add(hostScanDeadline))

		err = encoding.WriteObject(conn, modules.RPCSettings)
		if err != nil {
			return err
		}
		var pubkey crypto.PublicKey
		copy(pubkey[:], pubKey.Key)
		return crypto.ReadSignedObject(conn, &settings, maxSettingsLen, pubkey)
	}()
	if err != nil {
		hdb.log.Debugf("Scan of host at %v failed: %v", netAddr, err)
	} else {
		hdb.log.Debugf("Scan of host at %v succeeded.", netAddr)
		entry.HostExternalSettings = settings
	}

	// Update the host tree to have a new entry, including the new error. Then
	// delete the entry from the scan map as the scan has been successful.
	hdb.mu.Lock()
	hdb.updateEntry(entry, err)
	hdb.mu.Unlock()
}

// threadedProbeHosts pulls hosts from the thread pool and runs a scan on them.
func (hdb *HostDB) threadedProbeHosts() {
	err := hdb.tg.Add()
	if err != nil {
		return
	}
	defer hdb.tg.Done()

	for {
		select {
		case <-hdb.tg.StopChan():
			return
		case hostEntry := <-hdb.scanPool:
			// Block the scan until the host is online.
			for {
				hdb.mu.RLock()
				online := hdb.online
				hdb.mu.RUnlock()
				if online {
					break
				}

				// Check again in 30 seconds.
				select {
				case <-time.After(time.Second * 30):
					continue
				case <-hdb.tg.StopChan():
					return
				}
			}

			// There appears to be internet connectivity, continue with the
			// scan.
			hdb.managedScanHost(hostEntry)
		}
	}
}

// threadedScan is an ongoing function which will query the full set of hosts
// every few hours to see who is online and available for uploading.
func (hdb *HostDB) threadedScan() {
	err := hdb.tg.Add()
	if err != nil {
		return
	}
	defer hdb.tg.Done()

	for {
		// Set up a scan for the hostCheckupQuanity most valuable hosts in the
		// hostdb. Hosts that fail their scans will be docked significantly,
		// pushing them further back in the hierarchy, ensuring that for the
		// most part only online hosts are getting scanned unless there are
		// fewer than hostCheckupQuantity of them.

		// Grab a set of hosts to scan, grab hosts that are active, inactive,
		// and offline to get high diversity.
		var onlineHosts, offlineHosts []modules.HostDBEntry
		hosts := hdb.hostTree.All()
		for i := 0; i < len(hosts) && (len(onlineHosts) < hostCheckupQuantity || len(offlineHosts) < hostCheckupQuantity); i++ {
			// Figure out if the host is online or offline.
			online := false
			scanLen := len(hosts[i].ScanHistory)
			if scanLen > 0 && hosts[i].ScanHistory[scanLen-1].Success {
				online = true
			}

			if online && len(onlineHosts) < hostCheckupQuantity {
				onlineHosts = append(onlineHosts, hosts[i])
			} else if !online && len(offlineHosts) < hostCheckupQuantity {
				offlineHosts = append(onlineHosts, hosts[i])
			}
		}

		// Queue the scans for each host.
		hdb.log.Println("Performing scan on", len(onlineHosts), "online hosts and", len(offlineHosts), "offline hosts.")
		hdb.mu.Lock()
		for _, host := range onlineHosts {
			hdb.queueScan(host)
		}
		for _, host := range offlineHosts {
			hdb.queueScan(host)
		}
		hdb.mu.Unlock()

		// Sleep for a random amount of time before doing another round of
		// scanning. The minimums and maximums keep the scan time reasonable,
		// while the randomness prevents the scanning from always happening at
		// the same time of day or week.
		maxBig := big.NewInt(int64(maxScanSleep))
		minBig := big.NewInt(int64(minScanSleep))
		randSleep, err := rand.Int(rand.Reader, maxBig.Sub(maxBig, minBig))
		if err != nil {
			build.Critical(err)
			// If there's an error, sleep for the default amount of time.
			defaultBig := big.NewInt(int64(defaultScanSleep))
			randSleep = defaultBig.Sub(defaultBig, minBig)
		}

		// Sleep until it's time for the next scan cycle.
		select {
		case <-hdb.tg.StopChan():
			return
		case <-time.After(time.Duration(randSleep.Int64()) + minScanSleep):
		}
	}
}
