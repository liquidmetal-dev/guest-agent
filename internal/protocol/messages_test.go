package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRequestPutJSON(t *testing.T) {
	req := Request{
		Version: Version,
		Op:      OpPut,
		Put: &Put{
			Path:         "/tmp/x",
			Mode:         0o640,
			User:         "1000",
			Group:        "wheel",
			MkdirParents: true,
			TimeoutSec:   5,
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"version":1,"op":"put","put":{"path":"/tmp/x","mode":416,"user":"1000","group":"wheel","mkdir_parents":true,"timeout_sec":5}}`
	if string(b) != want {
		t.Errorf("json = %s\nwant   %s", b, want)
	}
	var back Request
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Put == nil || *back.Put != *req.Put {
		t.Errorf("round trip = %+v, want %+v", back.Put, req.Put)
	}
}

func TestRequestGetJSON(t *testing.T) {
	req := Request{Version: Version, Op: OpGet, Get: &Get{Path: "/etc/hostname", TimeoutSec: 3}}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"version":1,"op":"get","get":{"path":"/etc/hostname","timeout_sec":3}}`
	if string(b) != want {
		t.Errorf("json = %s\nwant   %s", b, want)
	}
}

func TestWriteFileInfoRoundTrip(t *testing.T) {
	fi := FileInfo{Size: 12, Mode: 0o755, UID: 1000, GID: 100, Mtime: "2026-10-14T12:00:00Z"}
	var buf bytes.Buffer
	if err := WriteFileInfo(&buf, fi); err != nil {
		t.Fatalf("WriteFileInfo: %v", err)
	}
	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if f.Type != FrameFileInfo {
		t.Fatalf("type = %s, want file_info", f.Type)
	}
	var back FileInfo
	if err := json.Unmarshal(f.Payload, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != fi {
		t.Errorf("round trip = %+v, want %+v", back, fi)
	}
	want := `{"size":12,"mode":493,"uid":1000,"gid":100,"mtime":"2026-10-14T12:00:00Z"}`
	if string(f.Payload) != want {
		t.Errorf("json = %s\nwant   %s", f.Payload, want)
	}
}

func TestInfoMessageOpsJSON(t *testing.T) {
	b, err := json.Marshal(InfoMessage{Version: "v", Uname: "u", Uptime: "1s", Ops: []string{"exec", "put"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"version":"v","uname":"u","uptime":"1s","ops":["exec","put"]}`
	if string(b) != want {
		t.Errorf("json = %s\nwant   %s", b, want)
	}
}
