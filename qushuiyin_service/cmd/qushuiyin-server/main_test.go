package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMockReport(t *testing.T) {
	env := map[string]string{"APP_ENV": "test", "PARSE_MODE": "mock"}
	get := func(k string) string { return env[k] }
	var b bytes.Buffer
	e := run(t.Context(), []string{"probe", "https://www.douyin.com/video/10001"}, get, &b)
	if e != nil {
		t.Fatal(e)
	}
	var rep report
	if json.Unmarshal(b.Bytes(), &rep) != nil || rep.Count != 1 || len(rep.Resources) != 2 || rep.Resources[0].Direct || rep.Resources[0].Host != "media.example.test" || strings.Contains(b.String(), "https://") {
		t.Fatal("unsafe or invalid report")
	}
	e = run(t.Context(), []string{"probe", "https://www.douyin.com/video/10003?sig=private"}, get, &b)
	if e == nil || strings.Contains(e.Error(), "private") {
		t.Fatal("unsafe error")
	}
	env["APP_ENV"] = "prod"
	if e := run(t.Context(), []string{"probe", "https://www.douyin.com/video/10001"}, get, &b); e == nil {
		t.Fatal("production probe allowed")
	}
}
