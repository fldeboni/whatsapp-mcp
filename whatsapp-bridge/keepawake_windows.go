package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Keep-awake: on "modern standby" notebooks the machine goes to sleep when the screen turns off on idle, and the
// bridge stops answering until someone touches it. While enabled, the bridge tells Windows it needs the SYSTEM
// (not the display) awake - the same request a download manager or media player makes - so the screen still
// turns off but the notebook keeps running. Closing the lid / power button still sleep it.
//
// Config: <data dir>\keep-awake.json, e.g. {"enabled": true, "only_when_plugged_in": true}. Missing file = off.
// Re-read every minute, so editing it needs no restart.

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecState  = kernel32.NewProc("SetThreadExecutionState")
	procGetSystemPowerState = kernel32.NewProc("GetSystemPowerStatus")
)

type keepAwakeConfig struct {
	Enabled           bool  `json:"enabled"`
	OnlyWhenPluggedIn *bool `json:"only_when_plugged_in"`
}

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

func keepAwakePath() string {
	return filepath.Join(filepath.Dir(storeDir()), "keep-awake.json")
}

func pluggedIn() bool {
	var s systemPowerStatus
	if r, _, _ := procGetSystemPowerState.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return true // unknown: behave as plugged in (desktops report 1 anyway)
	}
	return s.ACLineStatus != 0 // 1 = AC, 255 = unknown
}

func wantKeepAwake() bool {
	data, err := os.ReadFile(keepAwakePath())
	if err != nil {
		return false
	}
	var cfg keepAwakeConfig
	if json.Unmarshal(data, &cfg) != nil || !cfg.Enabled {
		return false
	}
	if cfg.OnlyWhenPluggedIn == nil || *cfg.OnlyWhenPluggedIn {
		return pluggedIn()
	}
	return true
}

func startKeepAwake() {
	go func() {
		// The execution state belongs to the calling thread: pin this goroutine to one OS thread for good.
		runtime.LockOSThread()
		active := false
		for {
			want := wantKeepAwake()
			if want != active {
				flags := uintptr(esContinuous)
				if want {
					flags |= esSystemRequired
				}
				procSetThreadExecState.Call(flags)
				active = want
				if want {
					fmt.Println("keep-awake: on (screen may turn off, the notebook stays awake)")
				} else {
					fmt.Println("keep-awake: off (Windows may sleep normally)")
				}
			}
			time.Sleep(time.Minute)
		}
	}()
}
