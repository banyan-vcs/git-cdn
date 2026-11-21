package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
)

type route struct {
	id       string
	prefix   string
	base     *url.URL
	path     string
	repo     string
	endpoint string
}

func (s *Server) resolve(r *http.Request) (route, error) {
	var rt route
	if strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") {
		return rt, errors.New("encoded path separator")
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\\x00\r\n") {
			return rt, errors.New("invalid repository path")
		}
	}
	if s.config.Mode == config.ModeSingle {
		rt.id = config.SingleUpstreamID
	} else {
		if len(parts) < 2 {
			return rt, errors.New("missing upstream or repository")
		}
		rt.id = parts[0]
		rt.prefix = "/" + rt.id
		parts = parts[1:]
	}
	rt.base = s.upstreams[rt.id]
	if rt.base == nil {
		return rt, errors.New("unknown upstream")
	}
	boundary := -1
	switch {
	case len(parts) >= 3 && parts[len(parts)-2] == "info" && parts[len(parts)-1] == "refs":
		boundary = len(parts) - 2
	case len(parts) >= 2 && (parts[len(parts)-1] == "git-upload-pack" || parts[len(parts)-1] == "git-receive-pack"):
		boundary = len(parts) - 1
	default:
		for i := len(parts) - 2; i > 0; i-- {
			if parts[i] == "info" && parts[i+1] == "lfs" {
				boundary = i
				break
			}
		}
	}
	if boundary < 0 {
		for i, p := range parts {
			if strings.HasSuffix(p, ".git") {
				boundary = i + 1
				break
			}
		}
	}
	if boundary < 0 {
		boundary = len(parts)
	}
	name := strings.TrimSuffix(parts[boundary-1], ".git")
	if name == "" {
		return rt, errors.New("empty repository name")
	}
	parts[boundary-1] = name + ".git"
	rt.repo = strings.Join(parts[:boundary], "/")
	rt.path = strings.Join(parts, "/")
	rt.endpoint = strings.Join(parts[boundary:], "/")
	return rt, nil
}

func (rt route) target(path string) *url.URL {
	u := *rt.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + path
	u.RawPath, u.RawQuery = "", ""
	return &u
}

func rewriteLocation(rt route, response *http.Response) string {
	location := response.Header.Get("Location")
	if location == "" {
		return ""
	}
	u, err := response.Request.URL.Parse(location)
	prefix := strings.TrimSuffix(rt.base.Path, "/") + "/"
	if err != nil || u.User != nil || u.Scheme != rt.base.Scheme || u.Host != rt.base.Host || !strings.HasPrefix(u.Path, prefix) {
		return ""
	}
	u.Scheme, u.Host = "", ""
	u.Path = rt.prefix + "/" + strings.TrimPrefix(u.Path, prefix)
	u.RawPath = ""
	return u.String()
}
