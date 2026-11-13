//go:build linux && amd64

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

var statusFields = map[string]bool{"Tgid": true, "Pid": true, "PPid": true, "Uid": true, "Gid": true,
	"Groups": true, "CapInh": true, "CapPrm": true, "CapEff": true, "CapBnd": true, "CapAmb": true,
	"NoNewPrivs": true, "Seccomp": true, "Seccomp_filters": true, "TracerPid": true, "NSpid": true, "NStgid": true}

func boundedRead(path string) string {
	f, err := os.Open(path) // #nosec G304 -- Callers form only allowlisted procfs fields of positively tracked descendant task IDs.
	if err != nil {
		return unavailable
	}
	defer func() { _ = f.Close() }()
	var b [8193]byte
	n, err := f.Read(b[:])
	if err != nil || n > 8192 {
		return unavailable
	}
	return strings.TrimSpace(string(b[:n]))
}

func status(pid int) map[string]string {
	result := make(map[string]string)
	for line := range strings.SplitSeq(boundedRead(fmt.Sprintf("/proc/%d/status", pid)), "\n") {
		key, value, _ := strings.Cut(line, ":")
		if statusFields[key] {
			result[key] = strings.TrimSpace(value)
		}
	}
	return result
}

func taskTGID(pid int) int {
	value, _ := strconv.Atoi(status(pid)["Tgid"])
	return value
}

func imageIdentity(pid int) string {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d/exe", pid), &st); err != nil {
		return unavailable
	}
	return fmt.Sprintf("%d:%d:%d", st.Dev, st.Ino, st.Size)
}

func taskContext(pid int) map[string]any {
	root := fmt.Sprintf("/proc/%d/", pid)
	result := map[string]any{statusKey: status(pid), imageKey: imageIdentity(pid)}
	for _, field := range []string{"cgroup", "attr/current", "uid_map", "gid_map", "setgroups"} {
		result[field] = boundedRead(root + field)
	}
	result["stat"] = statFacts(boundedRead(root + "stat"))
	ns := make(map[string]string)
	for _, name := range []string{"user", "pid", "mnt", "cgroup"} {
		ns[name], _ = os.Readlink(root + "ns/" + name)
	}
	result["namespaces"], result["terminals"] = ns, terminalFacts(root)
	return result
}

func statFacts(value string) []string {
	end := strings.LastIndex(value, ") ")
	if end < 0 {
		return nil
	}
	fields := strings.Fields(value[end+2:])
	if len(fields) < 20 {
		return nil
	}
	// Omit comm, which can contain caller-controlled payloads.
	return append(fields[:6:6], fields[19])
}

func terminalFacts(root string) []map[string]any {
	var result []map[string]any
	directory, err := os.Open(root + "fd") // #nosec G304 -- Root is a positively tracked owned procfs task, never an arbitrary directory.
	if err != nil {
		return []map[string]any{{unavailable: "descriptor directory absent"}}
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(129)
	if err != nil || len(entries) > 128 {
		return []map[string]any{{unavailable: "descriptor directory absent or exceeds 128 entries"}}
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if entry := terminalFact(root, fd); entry != nil {
			result = append(result, entry)
		}
	}
	return result
}

func terminalFact(root string, fd int) map[string]any {
	path := fmt.Sprintf("%sfd/%d", root, fd)
	link, _ := os.Readlink(path)
	if !strings.HasPrefix(link, "/dev/pts/") && link != "/dev/ptmx" {
		return nil
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return nil
	}
	return map[string]any{"fd": fd, "terminal": link, "device": st.Rdev, "uid": st.Uid, "gid": st.Gid, "mode": st.Mode}
}

func decodeWords(data []byte) []uint64 {
	result := make([]uint64, len(data)/8)
	for i := range result {
		result[i] = binary.LittleEndian.Uint64(data[i*8:])
	}
	return result
}
