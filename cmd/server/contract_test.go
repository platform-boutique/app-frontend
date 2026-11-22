package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	// the generated stubs register their file descriptors in init()
	_ "frontend/genproto"
	_ "frontend/genproto/inventory"
	_ "frontend/genproto/mailbox"
	_ "frontend/genproto/orders"
	_ "frontend/genproto/users"
)

var updateContract = flag.Bool("update-contract", false, "rewrite testdata/grpc-contract.golden from the generated stubs")

// contractLines describes every service, method, message field and enum value of the gRPC contracts this
// service speaks (package hipstershop*), one line each, sorted. It is read from the descriptors compiled
// into the generated stubs, so it is what goes over the wire: service and method names, field numbers,
// names and types.
func contractLines() []string {
	var lines []string
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(fd.Package()), "hipstershop") {
			return true
		}
		for i := 0; i < fd.Services().Len(); i++ {
			s := fd.Services().Get(i)
			for j := 0; j < s.Methods().Len(); j++ {
				m := s.Methods().Get(j)
				stream := ""
				if m.IsStreamingClient() || m.IsStreamingServer() {
					stream = " stream"
				}
				lines = append(lines, fmt.Sprintf("rpc %s/%s(%s) returns (%s)%s", s.FullName(), m.Name(), m.Input().FullName(), m.Output().FullName(), stream))
			}
		}
		var messages func(ms protoreflect.MessageDescriptors)
		messages = func(ms protoreflect.MessageDescriptors) {
			for i := 0; i < ms.Len(); i++ {
				m := ms.Get(i)
				for j := 0; j < m.Fields().Len(); j++ {
					f := m.Fields().Get(j)
					typ := f.Kind().String()
					if f.Message() != nil {
						typ = string(f.Message().FullName())
					} else if f.Enum() != nil {
						typ = string(f.Enum().FullName())
					}
					card := f.Cardinality().String()
					if f.IsMap() {
						card = "map"
					}
					lines = append(lines, fmt.Sprintf("field %s.%s = %d %s %s", m.FullName(), f.Name(), f.Number(), card, typ))
				}
				messages(m.Messages())
			}
		}
		messages(fd.Messages())
		for i := 0; i < fd.Enums().Len(); i++ {
			e := fd.Enums().Get(i)
			for j := 0; j < e.Values().Len(); j++ {
				lines = append(lines, fmt.Sprintf("enum %s.%s = %d", e.FullName(), e.Values().Get(j).Name(), e.Values().Get(j).Number()))
			}
		}
		return true
	})
	sort.Strings(lines)
	return lines
}

// TestGRPCContract_Golden is the contract test of the gRPC clients (TEST-03): the wire contract of every
// backend this frontend calls is pinned in testdata/grpc-contract.golden. Contracts change additively
// and server first: a removed or renumbered line is a break and fails with a clear message; a new line
// (a new field or RPC the frontend now knows) fails until the golden file is updated on purpose.
func TestGRPCContract_Golden(t *testing.T) {
	const golden = "testdata/grpc-contract.golden"
	got := contractLines()
	if len(got) < 100 {
		t.Fatalf("only %d contract lines: the stubs did not register", len(got))
	}
	if *updateContract {
		if err := os.WriteFile(golden, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil { // #nosec G306 -- test data
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (create it with: go test ./cmd/server -run TestGRPCContract_Golden -update-contract)", err)
	}
	want := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	have := map[string]bool{}
	for _, l := range got {
		have[l] = true
	}
	pinned := map[string]bool{}
	for _, l := range want {
		pinned[l] = true
		if !have[l] {
			t.Errorf("BREAKING: contract line gone or changed: %s", l)
		}
	}
	for _, l := range got {
		if !pinned[l] {
			t.Errorf("new in the stubs (additive, update the golden file on purpose: go test ./cmd/server -run TestGRPCContract_Golden -update-contract): %s", l)
		}
	}
}
