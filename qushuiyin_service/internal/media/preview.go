package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
)

type PreviewStream struct {
	Body         io.ReadCloser
	Size         int64
	TotalSize    int64
	Status       int
	ContentRange string
}

// A preview is a bounded range, never a prepared download or a source URL outlet.
func (r *Relay) VideoPreview(ctx context.Context, platform string, a parse.Asset, header string, head bool, reserve func(int64) int64) (PreviewStream, error) {
	var out PreviewStream
	limits := r.Settings.Limits()
	o := (Probe{Client: r.Client, Limits: limits, Domains: r.Domains, Policy: "relay_only"}).Inspect(ctx, platform, a)
	if o.FinalURL == "" || o.Type != "video/mp4" || o.Size == nil || *o.Size <= 0 {
		return out, errors.New("FORMAT_UNSUPPORTED")
	}
	total := *o.Size
	out.TotalSize = total
	start, end, err := previewRange(header, total)
	if err != nil {
		return out, err
	}
	budget := reserve(end - start + 1)
	if budget <= 0 {
		return out, errors.New("RATE_LIMITED")
	}
	end = min(end, start+min(budget, total-start)-1)
	out.Size = end - start + 1
	out.Status = http.StatusOK
	if header != "" || out.Size != total {
		out.Status = http.StatusPartialContent
		out.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, total)
	}
	if head {
		out.Body = http.NoBody
		return out, nil
	}
	resp, err := r.Client.Headers(ctx, o.FinalURL, "GET", fmt.Sprintf("bytes=%d-%d", start, end), outbound.HTTPS)
	if err != nil {
		return out, err
	}
	valid := false
	if resp.StatusCode == 206 {
		m := partial.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if m != nil {
			s, e1 := strconv.ParseInt(m[1], 10, 64)
			e, e2 := strconv.ParseInt(m[2], 10, 64)
			n, e3 := strconv.ParseInt(m[3], 10, 64)
			valid = e1 == nil && e2 == nil && e3 == nil && s == start && e == end && n == total && (resp.ContentLength < 0 || resp.ContentLength == out.Size)
		}
	} else if resp.StatusCode == 200 {
		valid = start == 0 && (resp.ContentLength < 0 || resp.ContentLength == total)
	}
	mt, generic := contentType(resp)
	if !valid || (!generic && mt != "video/mp4") {
		resp.Body.Close()
		return out, errors.New("FORMAT_UNSUPPORTED")
	}
	var reader io.Reader = resp.Body
	if start == 0 {
		prefix, e := io.ReadAll(io.LimitReader(resp.Body, min(out.Size, 12)))
		if e != nil || (out.Size >= 12 && sniffMedia(bytes.NewReader(prefix), 12) != "video/mp4") {
			resp.Body.Close()
			return out, errors.New("FORMAT_UNSUPPORTED")
		}
		reader = io.MultiReader(bytes.NewReader(prefix), resp.Body)
	}
	out.Body = &previewBody{Reader: io.LimitReader(reader, out.Size), Closer: resp.Body}
	return out, nil
}

type previewBody struct {
	io.Reader
	io.Closer
}

func previewRange(header string, total int64) (int64, int64, error) {
	bad := errors.New("BAD_RANGE")
	if header == "" {
		return 0, total - 1, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, bad
	}
	p := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(p) != 2 {
		return 0, 0, bad
	}
	if p[0] == "" {
		n, e := strconv.ParseInt(p[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, bad
		}
		return max(0, total-n), total - 1, nil
	}
	start, e := strconv.ParseInt(p[0], 10, 64)
	if e != nil || start < 0 || start >= total {
		return 0, 0, bad
	}
	end := total - 1
	if p[1] != "" {
		end, e = strconv.ParseInt(p[1], 10, 64)
		if e != nil || end < start {
			return 0, 0, bad
		}
	}
	return start, min(end, total-1), nil
}
