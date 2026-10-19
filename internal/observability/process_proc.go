package observability

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type procStat struct {
	pid, parent     int
	start, cpu, rss uint64
}

// Linux exposes /proc stat times in USER_HZ (100), independently of the kernel
// scheduler CONFIG_HZ. StartTicks plus PID is the identity used for a sample.
const linuxUserHZ = 100

func readProcStat(root string, pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	// comm can contain spaces and parentheses; its closing ')' is the last one.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return procStat{}, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 22 {
		return procStat{}, fmt.Errorf("short process stat")
	}
	parse := func(i int) (uint64, error) { return strconv.ParseUint(fields[i], 10, 64) }
	parent, e1 := parse(1)
	user, e2 := parse(11)
	sys, e3 := parse(12)
	start, e4 := parse(19)
	rss, e5 := parse(21)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
		return procStat{}, fmt.Errorf("invalid process counters")
	}
	return procStat{pid: pid, parent: int(parent), start: start, cpu: user + sys, rss: rss * uint64(os.Getpagesize())}, nil
}

func collectProcess(root string, pid int, goos string) ProcessSnapshot {
	out := ProcessSnapshot{PID: pid}
	if goos != "linux" {
		return out
	}
	own, err := readProcStat(root, pid)
	if err != nil {
		return out
	}
	out.Available = true
	out.StartTicks = own.start
	out.CPUSeconds = float64(own.cpu) / linuxUserHZ
	out.RSSBytes = own.rss
	if data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "smaps_rollup")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == "Pss:" && f[2] == "kB" {
				if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					out.PSSBytes = v * 1024
					out.PSSAvailable = true
				}
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "io")); err == nil {
		read, write := false, false
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			v, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				continue
			}
			switch f[0] {
			case "read_bytes:":
				out.ReadBytes = v
				read = true
			case "write_bytes:":
				out.WriteBytes = v
				write = true
			}
		}
		out.IOAvailable = read && write
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	all := make(map[int]procStat)
	complete := true
	if len(entries) > 4096 {
		entries = entries[:4096]
		complete = false
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for _, entry := range entries {
		if time.Now().After(deadline) {
			complete = false
			break
		}
		child, err := strconv.Atoi(entry.Name())
		if err != nil || child == pid {
			continue
		}
		st, err := readProcStat(root, child)
		if err != nil {
			if !os.IsNotExist(err) {
				complete = false
			}
			continue
		}
		all[child] = st
	}
	descendants := map[int]bool{pid: true}
	for changed := true; changed; {
		changed = false
		for id, st := range all {
			if !descendants[id] && descendants[st.parent] {
				descendants[id] = true
				changed = true
			}
		}
	}
	for id := range descendants {
		if id == pid {
			continue
		}
		st := all[id]
		again, err := readProcStat(root, id)
		if err != nil || again.start != st.start || again.parent != st.parent {
			complete = false
			continue
		}
		out.Children++
		out.ChildrenRSSBytes += st.rss
		out.ChildrenCPUSeconds += float64(st.cpu) / linuxUserHZ
	}
	// A recycled own PID invalidates all attribution. A disappearing child only
	// marks the descendant snapshot partial; procfs is not an atomic census.
	again, err := readProcStat(root, pid)
	if err != nil || again.start != own.start {
		return ProcessSnapshot{PID: pid}
	}
	out.ChildrenComplete = complete
	return out
}
