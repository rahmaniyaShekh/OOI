package relay

import (
	"bytes"
	"testing"
)

func TestSealOpenDirections(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	aead, err := NewAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := Seal(aead, false, TypeViewerMedia, []byte("frame"))
	if err != nil {
		t.Fatal(err)
	}
	typ, p, err := Open(aead, false, msg)
	if err != nil || typ != TypeViewerMedia || string(p) != "frame" {
		t.Fatalf("open: %v %d %q", err, typ, p)
	}
	// A page-to-host message reflected back as if from the host must fail.
	if _, _, err := Open(aead, true, msg); err == nil {
		t.Fatal("reflected message was accepted")
	}
	msg[len(msg)-1] ^= 1
	if _, _, err := Open(aead, false, msg); err == nil {
		t.Fatal("tampered message was accepted")
	}
	if _, err := NewAEAD(key[:16]); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestAssembler(t *testing.T) {
	var a Assembler
	f, ok, err := a.Push(EncodeMedia(FlagKey, 10, []byte("whole")))
	if err != nil || !ok || !f.Key || f.TS != 10 || string(f.Data) != "whole" {
		t.Fatalf("single: %v %v %+v", err, ok, f)
	}
	if _, ok, _ := a.Push(EncodeMedia(FlagMore, 20, []byte("ab"))); ok {
		t.Fatal("partial frame emitted")
	}
	f, ok, _ = a.Push(EncodeMedia(0, 20, []byte("cd")))
	if !ok || f.Key || string(f.Data) != "abcd" {
		t.Fatalf("parts: %v %+v", ok, f)
	}
	// A new timestamp abandons an unfinished frame.
	a.Push(EncodeMedia(FlagKey|FlagMore, 30, []byte("x")))
	f, ok, _ = a.Push(EncodeMedia(0, 40, []byte("y")))
	if !ok || f.Key || string(f.Data) != "y" {
		t.Fatalf("abandon: %v %+v", ok, f)
	}
	if _, _, err := a.Push([]byte{1, 2}); err == nil {
		t.Fatal("short message accepted")
	}
	bad := EncodeMedia(0, 1, []byte("abc"))
	bad[8] = 9
	if _, _, err := a.Push(bad); err == nil {
		t.Fatal("bad length accepted")
	}
}
