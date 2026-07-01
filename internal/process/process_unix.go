//go:build unix

package process

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func prepareCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func commandStartsProcessGroup(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}

func killTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(process.Pid)
	ownedGroup := err == nil && pgid == process.Pid && pgid > 0
	if ownedGroup {
		target := -pgid
		_ = signalTarget(target, syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		_ = signalTarget(target, syscall.SIGKILL)
		return nil
	}

	pids := descendantPIDs(process.Pid)
	for _, pid := range pids {
		_ = signalTarget(pid, syscall.SIGTERM)
	}
	_ = signalTarget(process.Pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	for _, pid := range pids {
		_ = signalTarget(pid, syscall.SIGKILL)
	}
	_ = signalTarget(process.Pid, syscall.SIGKILL)
	return nil
}

func signalTarget(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(pid, sig); err != nil && err != syscall.ESRCH && err != syscall.EPERM {
		return err
	}
	return nil
}

func descendantPIDs(root int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := make(map[int][]int)
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	var result []int
	var walk func(int)
	walk = func(pid int) {
		for _, child := range children[pid] {
			walk(child)
			result = append(result, child)
		}
	}
	walk(root)
	return result
}
