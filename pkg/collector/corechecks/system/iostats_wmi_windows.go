// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2017 Datadog, Inc.

package system

import (
	"bytes"
	"fmt"
	"regexp"
	"syscall"
	"unsafe"

	"github.com/DataDog/datadog-agent/pkg/aggregator"
	"github.com/DataDog/datadog-agent/pkg/collector/check"
	"github.com/StackExchange/wmi"
	log "github.com/cihub/seelog"
)

var (
	modkernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDriveStringsW = modkernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveType            = modkernel32.NewProc("GetDriveTypeW")
)

// Win32PerfRawDataPerfDiskLogicalDisk contains perf infos for a drive
type Win32PerfRawDataPerfDiskLogicalDisk struct {
	CurrentDiskQueueLength uint32
	DiskReadBytesPerSec    uint64
	DiskReadsPerSec        uint32
	DiskWriteBytesPerSec   uint64
	DiskWritesPerSec       uint32
	FrequencySys100NS      uint64
	Name                   string
	TimestampSys100NS      uint64
}

// IOCheck doesn't need additional fields
type IOCheck struct {
	blacklist    *regexp.Regexp
	drivemap     map[string]Win32PerfRawDataPerfDiskLogicalDisk
	lastWarnings []error
}

// Configure the IOstats check
func (c *IOCheck) Configure(data check.ConfigData, initConfig check.ConfigData) error {
	err := error(nil)
	err = c.commonConfigure(data, initConfig)
	if err != nil {
		return err
	}

	c.drivemap = make(map[string]Win32PerfRawDataPerfDiskLogicalDisk, 0)

	drivebuf := make([]uint16, 256)

	// Windows API GetLogicalDriveStrings returns all of the assigned drive letters
	// https://msdn.microsoft.com/en-us/library/windows/desktop/aa364975(v=vs.85).aspx
	r, _, err := procGetLogicalDriveStringsW.Call(
		uintptr(len(drivebuf)),
		uintptr(unsafe.Pointer(&drivebuf[0])))
	if r == 0 {
		log.Errorf("IO Factory failed to get drive strings")
		return err
	}
	drivelist := convertWindowsStringList(drivebuf)
	for _, drive := range drivelist {
		r, _, _ = procGetDriveType.Call(uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(drive + "\\"))))
		if r != DRIVE_FIXED {
			continue
		}
		c.drivemap[drive] = Win32PerfRawDataPerfDiskLogicalDisk{}
	}
	return error(nil)
}

func computeValue(pvs Win32PerfRawDataPerfDiskLogicalDisk, cur *Win32PerfRawDataPerfDiskLogicalDisk) (ret map[string]float64, e error) {

	e = nil
	ret = make(map[string]float64, 0)
	f := pvs.FrequencySys100NS
	dt := cur.TimestampSys100NS - pvs.TimestampSys100NS
	log.Infof("DeltaT is %d (%d)", dt/10000000, dt)

	if f == 0 {
		log.Errorf("Frequency is zero?")
		return nil, fmt.Errorf("Divide by zero (frequency)")
	}
	if dt == 0 {
		log.Errorf("delta-T is zero?")
		return nil, fmt.Errorf("Divide by zero (delta-T)")
	}

	v := (cur.DiskWriteBytesPerSec - pvs.DiskWriteBytesPerSec) / (dt / f)
	ret["system.io.wkb_s"] = float64(v / 1024)

	v = (uint64(cur.DiskWritesPerSec) - uint64(pvs.DiskWritesPerSec)) / (dt / f)
	ret["system.io.w_s"] = float64(v)

	v = (cur.DiskReadBytesPerSec - pvs.DiskReadBytesPerSec) / (dt / f)
	ret["system.io.rkb_s"] = float64(v / 1024)

	v = (uint64(cur.DiskReadsPerSec) - uint64(pvs.DiskReadsPerSec)) / (dt / f)
	ret["system.io.r_s"] = float64(v)

	v = (uint64(cur.CurrentDiskQueueLength) - uint64(pvs.CurrentDiskQueueLength)) / (dt / f)
	ret["system.io.avg_q_sz"] = float64(v)

	return ret, e

}

// Run executes the check
func (c *IOCheck) Run() error {
	sender, err := aggregator.GetSender(c.ID())
	if err != nil {
		return err
	}

	var dst []Win32PerfRawDataPerfDiskLogicalDisk
	err = wmi.Query("SELECT Name, DiskWriteBytesPerSec, DiskWritesPerSec, DiskReadBytesPerSec, DiskReadsPerSec, CurrentDiskQueueLength, TimestampSys100NS, FrequencySys100NS FROM Win32PerfRawDataPerfDiskLogicalDisk ", &dst)
	if err != nil {
		log.Errorf("Error in WMI query %s", err.Error())
		return err
	}
	var tagbuff bytes.Buffer
	for _, d := range dst {
		log.Debugf("Got drive %s", d.Name)
		if len(d.Name) > 3 {
			continue
		}
		drive := d.Name
		if c.blacklist != nil && c.blacklist.MatchString(drive) {
			log.Debugf("matched drive %s against blacklist; skipping", drive)
			continue
		}

		tagbuff.Reset()
		tagbuff.WriteString("device:")
		tagbuff.WriteString(drive)
		tags := []string{tagbuff.String()}
		if prev, ok := c.drivemap[d.Name]; ok {
			// have a previous value we can compute from
			metrics, err := computeValue(prev, &d)
			if err != nil {
				log.Errorf("Error computing WMI statistics: %s", err)
			} else {
				for k, v := range metrics {
					log.Debugf("Setting %s to %f", k, v)
					sender.Gauge(k, v, "", tags)
				}
			}

		}
		c.drivemap[d.Name] = d
	}
	sender.Commit()
	return nil
}

func convertWindowsStringList(winput []uint16) []string {
	var retstrings []string
	var buffer bytes.Buffer

	for i := 0; i < (len(winput) - 1); i++ {
		if winput[i] == 0 {
			retstrings = append(retstrings, buffer.String())
			buffer.Reset()

			if winput[i+1] == 0 {
				return retstrings
			}
			continue
		}
		buffer.WriteString(string(rune(winput[i])))
	}
	return retstrings
}
