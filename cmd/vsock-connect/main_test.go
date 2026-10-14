package main

import (
	"strings"
	"testing"
)

func TestParsePut(t *testing.T) {
	p, src, err := parsePut([]string{"--mode", "0600", "--owner", "ubuntu:dev", "--mkdir", "--timeout", "5", "src.txt", "/dst"})
	if err != nil {
		t.Fatalf("parsePut: %v", err)
	}
	if src != "src.txt" {
		t.Errorf("src = %q, want src.txt", src)
	}
	if p.Path != "/dst" || p.Mode != 0o600 || p.User != "ubuntu" || p.Group != "dev" || !p.MkdirParents || p.TimeoutSec != 5 {
		t.Errorf("put = %+v", p)
	}
}

func TestParsePutOwnerWithoutGroup(t *testing.T) {
	p, _, err := parsePut([]string{"--owner", "ubuntu", "-", "/dst"})
	if err != nil {
		t.Fatalf("parsePut: %v", err)
	}
	if p.User != "ubuntu" || p.Group != "" {
		t.Errorf("owner = %q:%q, want ubuntu and empty group", p.User, p.Group)
	}
}

func TestParsePutStdinSourceHasNoMode(t *testing.T) {
	p, src, err := parsePut([]string{"-", "/dst"})
	if err != nil {
		t.Fatalf("parsePut: %v", err)
	}
	if src != "-" || p.Mode != 0 {
		t.Errorf("src = %q mode = %o, want - and 0 (agent default)", src, p.Mode)
	}
}

func TestParsePutErrors(t *testing.T) {
	cases := map[string][]string{
		"missing dest":  {"src.txt"},
		"relative dest": {"src.txt", "dst.txt"},
		"bad mode":      {"--mode", "999", "src.txt", "/dst"},
		"unknown flag":  {"--bogus", "src.txt", "/dst"},
		"bad timeout":   {"--timeout", "-1", "src.txt", "/dst"},
		"extra arg":     {"src.txt", "/dst", "more"},
	}
	for name, args := range cases {
		if _, _, err := parsePut(args); err == nil {
			t.Errorf("%s: parsePut(%v) = nil error", name, args)
		}
	}
	if _, _, err := parsePut([]string{"src.txt", "dst.txt"}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative dest error = %v, want mention of absolute", err)
	}
}

func TestParseGet(t *testing.T) {
	g, dest, err := parseGet([]string{"--timeout", "3", "/src"})
	if err != nil {
		t.Fatalf("parseGet: %v", err)
	}
	if g.Path != "/src" || g.TimeoutSec != 3 || dest != "" {
		t.Errorf("get = %+v dest = %q", g, dest)
	}

	g, dest, err = parseGet([]string{"/src", "out.txt"})
	if err != nil {
		t.Fatalf("parseGet: %v", err)
	}
	if g.Path != "/src" || dest != "out.txt" {
		t.Errorf("get = %+v dest = %q", g, dest)
	}

	_, dest, err = parseGet([]string{"/src", "-"})
	if err != nil || dest != "-" {
		t.Errorf("dest = %q err = %v, want -", dest, err)
	}
}

func TestParseGetErrors(t *testing.T) {
	cases := map[string][]string{
		"no src":       {},
		"relative src": {"etc/hostname"},
		"bad timeout":  {"--timeout", "x", "/src"},
		"extra arg":    {"/src", "out", "more"},
		"unknown flag": {"--mkdir", "/src"},
	}
	for name, args := range cases {
		if _, _, err := parseGet(args); err == nil {
			t.Errorf("%s: parseGet(%v) = nil error", name, args)
		}
	}
}
