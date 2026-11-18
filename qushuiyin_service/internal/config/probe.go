// Package config owns the M0 subset of spec section 16.
package config

import "time"

type Limits struct {
	Redirects, Assets, Title, Description                       int
	RangeBytes, VideoBytes, ImageBytes                          int64
	Job, ParserCall, Media, Batch, AssumedTTL, Download, Margin time.Duration
}

func Defaults() Limits {
	return Limits{
		Redirects: 5, Assets: 100, Title: 200, Description: 10000,
		RangeBytes: 4096, VideoBytes: 104857600, ImageBytes: 20971520,
		Job: 25 * time.Second, ParserCall: 10 * time.Second, Media: 90 * time.Second, Batch: 180 * time.Second,
		AssumedTTL: 600 * time.Second, Download: 120 * time.Second, Margin: 30 * time.Second,
	}
}
