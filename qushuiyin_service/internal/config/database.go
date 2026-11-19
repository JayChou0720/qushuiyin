package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"time"
)

type Values struct {
	Data     map[string]json.RawMessage
	Versions map[string]int
}

func Load(ctx context.Context, db *sql.DB) (Values, error) {
	rows, err := db.QueryContext(ctx, "SELECT config_key,value_json,version FROM system_config ORDER BY config_key,version")
	if err != nil {
		return Values{}, err
	}
	defer rows.Close()
	out := Values{map[string]json.RawMessage{}, map[string]int{}}
	for rows.Next() {
		var k string
		var raw json.RawMessage
		var v int
		if err = rows.Scan(&k, &raw, &v); err != nil {
			return Values{}, err
		}
		out.Data[k] = raw
		out.Versions[k] = v
	}
	return out, rows.Err()
}
func (v Values) Int(key string) int64             { var n int64; _ = json.Unmarshal(v.Data[key], &n); return n }
func (v Values) Bool(key string) bool             { var b bool; _ = json.Unmarshal(v.Data[key], &b); return b }
func (v Values) Text(key string) string           { var s string; _ = json.Unmarshal(v.Data[key], &s); return s }
func (v Values) Seconds(key string) time.Duration { return time.Duration(v.Int(key)) * time.Second }
func (v Values) Validate(positiveKeys ...string) error {
	for _, k := range positiveKeys {
		if v.Int(k) <= 0 {
			return errors.New("missing or invalid system_config: " + k)
		}
	}
	return nil
}

func LoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func LoopbackOrigin(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || !LoopbackHost(u.Hostname()) || u.User != nil || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("expected an explicit loopback HTTP origin")
	}
	return u, nil
}
