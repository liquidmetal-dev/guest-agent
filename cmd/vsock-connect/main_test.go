package main

import (
	"reflect"
	"testing"
)

func TestParseConnStopsAtCommandSeparator(t *testing.T) {
	c, rest, err := parseConn([]string{
		"--tcp", "127.0.0.1:1024",
		"--", "echo", "--tcp", "not-an-addr", "--port", "1234",
	})
	if err != nil {
		t.Fatalf("parseConn: %v", err)
	}
	if c.tcp != "127.0.0.1:1024" {
		t.Fatalf("tcp = %q, want 127.0.0.1:1024", c.tcp)
	}
	want := []string{"--", "echo", "--tcp", "not-an-addr", "--port", "1234"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
}

func TestParseConnRejectsInvalidPort(t *testing.T) {
	if _, _, err := parseConn([]string{"--uds", "/tmp/vm.vsock", "--port", "nope"}); err == nil {
		t.Fatal("parseConn accepted invalid port")
	}
	if _, _, err := parseConn([]string{"--uds", "/tmp/vm.vsock", "--port", "0"}); err == nil {
		t.Fatal("parseConn accepted zero port")
	}
}

func TestParseExecPreservesFlagLikeCommandArgs(t *testing.T) {
	e, err := parseExec([]string{"--timeout", "5", "--", "echo", "--tcp", "not-an-addr"})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	if e.TimeoutSec != 5 {
		t.Fatalf("timeout = %d, want 5", e.TimeoutSec)
	}
	if e.Cmd != "echo" {
		t.Fatalf("cmd = %q, want echo", e.Cmd)
	}
	wantArgs := []string{"--tcp", "not-an-addr"}
	if !reflect.DeepEqual(e.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", e.Args, wantArgs)
	}
}

func TestParseExecRejectsInvalidTimeout(t *testing.T) {
	for _, args := range [][]string{
		{"--timeout", "nope", "--", "echo"},
		{"--timeout", "-1", "--", "echo"},
	} {
		if _, err := parseExec(args); err == nil {
			t.Fatalf("parseExec(%#v) accepted invalid timeout", args)
		}
	}
}
