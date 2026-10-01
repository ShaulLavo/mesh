package serve

import (
	"fmt"
	"sort"
	"strings"
)

type PrivateRouteShadow struct {
	Private Service
	Public  Service
}

func (s PrivateRouteShadow) Message() string {
	return fmt.Sprintf("private route /%s shadows public route https://%s/%s at /%s; public requests there return 404",
		s.Private.Name, s.Public.PublicName, s.Public.Name, s.Private.Name)
}

func PrivateRouteShadows(services []Service) []PrivateRouteShadow {
	var shadows []PrivateRouteShadow
	for _, private := range services {
		if private.LocalOnly || private.PublicName != "" {
			continue
		}
		for _, public := range services {
			if public.LocalOnly || public.PublicName == "" || !strings.HasPrefix(private.Name, public.Name+"/") {
				continue
			}
			shadows = append(shadows, PrivateRouteShadow{Private: private, Public: public})
		}
	}
	sort.Slice(shadows, func(i, j int) bool {
		if shadows[i].Private.Name != shadows[j].Private.Name {
			return shadows[i].Private.Name < shadows[j].Private.Name
		}
		return shadows[i].Public.Name < shadows[j].Public.Name
	})
	return shadows
}
