package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSend(t *testing.T) {
	var got struct {
		path, auth string
		body       map[string]string
	}
	code := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.String(), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		w.WriteHeader(code)
	}))
	defer srv.Close()
	env := []string{"STELLWERK_URL=" + srv.URL, "STELLWERK_TOKEN=tok"}
	var out strings.Builder
	err := runSend([]string{"-source", "backup", "-title", "Backup", "-status", "failed", "-label", "host=nas", "restic", "says", "no"}, nil, &out, env)
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/in/backup?host=nas" || got.auth != "Bearer tok" || got.body["message"] != "restic says no" || got.body["title"] != "Backup" ||
		got.body["status"] != "failed" || out.String() != "delivered\n" {
		t.Errorf("%+v %q", got, out.String())
	}
	if err := runSend([]string{"-source", "b", "-"}, strings.NewReader("  from stdin \n"), &out, env); err != nil || got.body["message"] != "from stdin" {
		t.Errorf("stdin: %v %+v", err, got.body)
	}
	code = 401
	if err := runSend([]string{"-source", "b", "x"}, nil, &out, env); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("%v", err)
	}
	for name, args := range map[string][]string{
		"no source":  {"x"},
		"no message": {"-source", "b"},
		"bad label":  {"-source", "b", "-label", "nolabel", "x"},
		"bad flag":   {"-nope"},
	} {
		if err := runSend(args, nil, io.Discard, env); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := runSend([]string{"-source", "b", "-url", "http://127.0.0.1:1", "x"}, nil, io.Discard, nil); err == nil {
		t.Error("connection refused")
	}
}
