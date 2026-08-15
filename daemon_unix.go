//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || plan9

package daemon

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const stageVar = "__DAEMON_STAGE"

func Start(args []string) (int, error) {
	stage, origValue := getStage()
	advanceStage := func() error {
		base := fmt.Sprintf("%d/%09d/", stage+1, time.Now().Nanosecond())
		hash := sha1.New()
		hash.Write([]byte(base))
		tag := base + hex.EncodeToString(hash.Sum([]byte{}))

		if err := os.Setenv(stageVar, tag+":"+origValue); err != nil {
			return fmt.Errorf("can't set %s: %s", stageVar, err)
		}
		return nil
	}
	resetEnv := func() error {
		return os.Setenv(stageVar, origValue)
	}
	files := make([]*os.File, 3, 5)
	if stage == 0 {
		nullDev, err := os.OpenFile(os.DevNull, 0, 0)
		if err != nil {
			return 0, err
		}
		files[0], files[1], files[2] = nullDev, nullDev, nullDev
	} else {
		files[0], files[1], files[2] = os.Stdin, os.Stdout, os.Stderr
	}

	if stage < 2 {
		if err := advanceStage(); err != nil {
			return 0, err
		}
		dir, err := os.Getwd()
		if err != nil {
			return 0, err
		}
		osAttrs := os.ProcAttr{Dir: dir, Env: os.Environ(), Files: files}

		if stage == 0 {
			sysAttrs := syscall.SysProcAttr{Setsid: true}
			osAttrs.Sys = &sysAttrs
		}

		proc, err := os.StartProcess(_path, append([]string{_path}, args...), &osAttrs)
		if err != nil {
			return 0, err
		}
		pid := proc.Pid

		err = proc.Release()
		if err != nil {
			return 0, err
		}

		return pid, nil
	}

	//os.Chdir("/")
	syscall.Umask(0)
	err := resetEnv()
	if err != nil {
		return 0, err
	}

	currStage = stage
	return 0, nil
}

func Stop(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("can't find process %d: %s", pid, err)
	}
	err = process.Signal(os.Interrupt)
	if err != nil {
		return fmt.Errorf("can't send interrupt to process %d: %s", pid, err)
	}
	return nil
}

func Kill(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("can't find process %d: %s", pid, err)
	}
	err = process.Signal(os.Kill)
	if err != nil {
		return fmt.Errorf("can't send interrupt to process %d: %s", pid, err)
	}
	return nil
}

// currStage keeps the current stage. This is used only as a cache for Stage(),
// in order to extend a valid result after MakeDaemon() has returned, where the
// environment variable would have already been reset. (Also, this is faster
// than repetitive calls to getStage().) Note that this approach is valid cause
// the stage doesn't change throughout any single process execution. It does
// only for the next process after the MakeDaemon() call.
var currStage = -1

// Stage returns the "stage of daemonizing", i.e., it allows you to know whether
// you're currently working in the parent, first child, or the final daemon.
// This is useless after the call to MakeDaemon(), cause that call will only
// return for the daemon stage. However, you can still use Stage() to tell
// whether you've daemonized or not, in case you have a running path that may
// exclude the call to MakeDaemon().
func Stage() int {
	if currStage == -1 {
		s, _ := getStage()
		currStage = s
	}
	return currStage
}

// Returns the current stage in the "daemonization process", that's kept in
// an environment variable. The variable is instrumented with a digital
// signature, to avoid misbehavior if it was present in the user's
// environment. The original value is restored after the last stage, so that
// there's no final effect on the environment the application receives.
func getStage() (stage int, origValue string) {
	stage = 0
	daemonStage := os.Getenv(stageVar)
	stageTag := strings.SplitN(daemonStage, ":", 2)
	stageInfo := strings.SplitN(stageTag[0], "/", 3)
	if len(stageInfo) == 3 {
		stageStr, tm, check := stageInfo[0], stageInfo[1], stageInfo[2]
		hash := sha1.New()
		hash.Write([]byte(stageStr + "/" + tm + "/"))
		if check != hex.EncodeToString(hash.Sum([]byte{})) {
			origValue = daemonStage
		} else {
			stage, _ = strconv.Atoi(stageStr)
			if len(stageTag) == 2 {
				origValue = stageTag[1]
			}
		}
	} else {
		origValue = daemonStage
	}
	return stage, origValue
}
