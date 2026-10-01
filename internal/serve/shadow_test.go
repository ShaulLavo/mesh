package serve

import (
	"reflect"
	"testing"
)

func TestPrivateRouteShadows(t *testing.T) {
	public := Service{Name: "blog", PublicName: "blog.shaulavo.dev"}
	private := Service{Name: "blog/admin"}
	for _, test := range []struct {
		name     string
		services []Service
		want     []PrivateRouteShadow
	}{
		{name: "public parent", services: []Service{private, public}, want: []PrivateRouteShadow{{Private: private, Public: public}}},
		{name: "private parent", services: []Service{{Name: "blog"}, private}},
		{name: "path segment boundary", services: []Service{public, {Name: "blogging/admin"}}},
		{name: "public child", services: []Service{public, {Name: "blog/admin", PublicName: public.PublicName}}},
		{name: "other public child", services: []Service{public, {Name: "blog/admin", PublicName: "admin.shaulavo.dev"}}},
		{name: "listener only", services: []Service{{Name: "3000", PublicName: public.PublicName}, {Name: "3000/admin", LocalOnly: true}}},
		{name: "private ancestor does not shadow child", services: []Service{{Name: "blog"}, {Name: "blog/admin", PublicName: public.PublicName}}},
		{name: "all public ancestors", services: []Service{private, {Name: "blog/admin/sub"}, public}, want: []PrivateRouteShadow{
			{Private: private, Public: public},
			{Private: Service{Name: "blog/admin/sub"}, Public: public},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PrivateRouteShadows(test.services); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("shadows = %#v, want %#v", got, test.want)
			}
		})
	}
}
