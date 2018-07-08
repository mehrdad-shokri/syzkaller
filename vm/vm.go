// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package vm provides an abstract test machine (VM, physical machine, etc)
// interface for the rest of the system.
// For convenience test machines are subsequently collectively called VMs.
// Package wraps vmimpl package interface with some common functionality
// and higher-level interface.
package vm

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/report"
	"github.com/google/syzkaller/syz-manager/mgrconfig"
	"github.com/google/syzkaller/vm/vmimpl"

	// Import all VM implementations, so that users only need to import vm.
	_ "github.com/google/syzkaller/vm/adb"
	_ "github.com/google/syzkaller/vm/gce"
	_ "github.com/google/syzkaller/vm/gvisor"
	_ "github.com/google/syzkaller/vm/isolated"
	_ "github.com/google/syzkaller/vm/kvm"
	_ "github.com/google/syzkaller/vm/odroid"
	_ "github.com/google/syzkaller/vm/qemu"
)

type Pool struct {
	impl    vmimpl.Pool
	workdir string
}

type Instance struct {
	impl    vmimpl.Instance
	workdir string
	index   int
}

var (
	Shutdown   = vmimpl.Shutdown
	ErrTimeout = vmimpl.ErrTimeout
)

type BootErrorer interface {
	BootError() (string, []byte)
}

func Create(cfg *mgrconfig.Config, debug bool) (*Pool, error) {
	env := &vmimpl.Env{
		Name:    cfg.Name,
		OS:      cfg.TargetOS,
		Arch:    cfg.TargetVMArch,
		Workdir: cfg.Workdir,
		Image:   cfg.Image,
		SSHKey:  cfg.SSHKey,
		SSHUser: cfg.SSHUser,
		Debug:   debug,
		Config:  cfg.VM,
	}
	impl, err := vmimpl.Create(cfg.Type, env)
	if err != nil {
		return nil, err
	}
	return &Pool{
		impl:    impl,
		workdir: env.Workdir,
	}, nil
}

func (pool *Pool) Count() int {
	return pool.impl.Count()
}

func (pool *Pool) Create(index int) (*Instance, error) {
	if index < 0 || index >= pool.Count() {
		return nil, fmt.Errorf("invalid VM index %v (count %v)", index, pool.Count())
	}
	workdir, err := osutil.ProcessTempDir(pool.workdir)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance temp dir: %v", err)
	}
	impl, err := pool.impl.Create(workdir, index)
	if err != nil {
		os.RemoveAll(workdir)
		return nil, err
	}
	return &Instance{
		impl:    impl,
		workdir: workdir,
		index:   index,
	}, nil
}

func (inst *Instance) Copy(hostSrc string) (string, error) {
	return inst.impl.Copy(hostSrc)
}

func (inst *Instance) Forward(port int) (string, error) {
	return inst.impl.Forward(port)
}

func (inst *Instance) Run(timeout time.Duration, stop <-chan bool, command string) (
	outc <-chan []byte, errc <-chan error, err error) {
	return inst.impl.Run(timeout, stop, command)
}

func (inst *Instance) Diagnose() bool {
	return inst.impl.Diagnose()
}

func (inst *Instance) Close() {
	inst.impl.Close()
	os.RemoveAll(inst.workdir)
}

// MonitorExecution monitors execution of a program running inside of a VM.
// It detects kernel oopses in output, lost connections, hangs, etc.
// outc/errc is what vm.Instance.Run returns, reporter parses kernel output for oopses.
// If canExit is false and the program exits, it is treated as an error.
// Returns a non-symbolized crash report, or nil if no error happens.
func (inst *Instance) MonitorExecution(outc <-chan []byte, errc <-chan error,
	reporter report.Reporter, canExit bool) (
	rep *report.Report) {
	var output []byte
	waitForOutput := func() {
		timer := time.NewTimer(10 * time.Second).C
		for {
			select {
			case out, ok := <-outc:
				if !ok {
					return
				}
				output = append(output, out...)
			case <-timer:
				return
			}
		}
	}

	matchPos := 0
	const (
		beforeContext = 1024 << 10
		afterContext  = 128 << 10
	)
	extractError := func(defaultError string) *report.Report {
		// Give it some time to finish writing the error message.
		waitForOutput()
		if bytes.Contains(output, []byte("SYZ-FUZZER: PREEMPTED")) {
			return nil
		}
		if !reporter.ContainsCrash(output[matchPos:]) {
			if defaultError == "" {
				if canExit {
					return nil
				}
				defaultError = "lost connection to test machine"
			}
			rep := &report.Report{
				Title:      defaultError,
				Output:     output,
				Suppressed: report.IsSuppressed(reporter, output),
			}
			return rep
		}
		rep := reporter.Parse(output[matchPos:])
		if rep == nil {
			panic(fmt.Sprintf("reporter.ContainsCrash/Parse disagree:\n%s", output[matchPos:]))
		}
		start := matchPos + rep.StartPos - beforeContext
		if start < 0 {
			start = 0
		}
		end := matchPos + rep.EndPos + afterContext
		if end > len(output) {
			end = len(output)
		}
		rep.Output = output[start:end]
		rep.StartPos += matchPos - start
		rep.EndPos += matchPos - start
		return rep
	}

	lastExecuteTime := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-errc:
			switch err {
			case nil:
				// The program has exited without errors,
				// but wait for kernel output in case there is some delayed oops.
				return extractError("")
			case ErrTimeout:
				return nil
			default:
				// Note: connection lost can race with a kernel oops message.
				// In such case we want to return the kernel oops.
				return extractError("lost connection to test machine")
			}
		case out := <-outc:
			lastPos := len(output)
			output = append(output, out...)
			if bytes.Contains(output[lastPos:], executingProgram1) ||
				bytes.Contains(output[lastPos:], executingProgram2) {
				lastExecuteTime = time.Now()
			}
			if reporter.ContainsCrash(output[matchPos:]) {
				return extractError("unknown error")
			}
			if len(output) > 2*beforeContext {
				copy(output, output[len(output)-beforeContext:])
				output = output[:beforeContext]
			}
			matchPos = len(output) - 512
			if matchPos < 0 {
				matchPos = 0
			}
		case <-ticker.C:
			// Detect both "not output whatsoever" and "kernel episodically prints
			// something to console, but fuzzer is not actually executing programs".
			// The timeout used to be 3 mins for a long time.
			// But (1) we were seeing flakes on linux where net namespace
			// destruction can be really slow, and (2) gVisor watchdog timeout
			// is 3 mins + 1/4 of that for checking period = 3m45s.
			// Current linux max timeout is CONFIG_DEFAULT_HUNG_TASK_TIMEOUT=140
			// and workqueue.watchdog_thresh=140 which both actually result
			// in 140-280s detection delay.
			// So the current timeout is 5 mins (300s).
			// We don't want it to be too long too because it will waste time on real hangs.
			if time.Since(lastExecuteTime) < 5*time.Minute {
				break
			}
			if inst.Diagnose() {
				waitForOutput()
			}
			rep := &report.Report{
				Title:      "no output from test machine",
				Output:     output,
				Suppressed: report.IsSuppressed(reporter, output),
			}
			return rep
		case <-Shutdown:
			return nil
		}
	}
}

var (
	executingProgram1 = []byte("executing program")  // syz-fuzzer output
	executingProgram2 = []byte("executed programs:") // syz-execprog output
)
