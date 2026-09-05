package wake

import (
	"errors"
	"strings"
)

func parseDarwinARP(contents []byte, route route) (string, error) {
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[1] != "("+route.gateway.String()+")" || fields[2] != "at" || fields[4] != "on" || fields[5] != route.device {
			continue
		}
		return normalizeMAC(fields[3])
	}
	return "", errors.New("default gateway has no matching Ethernet neighbor entry")
}
