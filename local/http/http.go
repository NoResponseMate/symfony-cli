/*
 * Copyright (c) 2021-present Fabien Potencier <fabien@symfony.com>
 *
 * This file is part of Symfony CLI project
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program. If not, see <http://www.gnu.org/licenses/>.
 */

package http

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/soheilhy/cmux"
	"github.com/symfony-cli/cert"
	"github.com/symfony-cli/symfony-cli/local/html"
	"github.com/symfony-cli/symfony-cli/local/process"
)

// ServerCallback serves non-static HTTP resources
type ServerCallback func(w http.ResponseWriter, r *http.Request, env map[string]string) error

// Server represents a server
type Server struct {
	DocumentRoot string
	Callback     ServerCallback
	Host         string
	PreferedPort int
	PKCS12       string
	AllowHTTP    bool
	Logger       zerolog.Logger
	Appversion   string

	httpserver  *http.Server
	httpsserver *http.Server

	serverPort string
}

// Start starts the server
func (s *Server) Start(errChan chan error) (int, error) {
	ln, port, err := process.CreateListener(s.Host, s.PreferedPort)
	if err != nil {
		return port, errors.WithStack(err)
	}
	s.serverPort = strconv.Itoa(port)

	s.httpserver = &http.Server{
		Handler: http.HandlerFunc(s.ProxyHandler),
	}
	if s.PKCS12 == "" {
		go func() {
			errChan <- errors.WithStack(s.httpserver.Serve(ln))
		}()

		return port, nil
	}

	cert, err := cert.Cert(s.PKCS12)
	if err != nil {
		return port, errors.WithStack(err)
	}

	s.httpsserver = &http.Server{
		Handler: http.HandlerFunc(s.ProxyHandler),
		TLSConfig: &tls.Config{
			PreferServerCipherSuites: true,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			Certificates:             []tls.Certificate{cert},
			NextProtos:               []string{"h2", "http/1.1"},
		},
	}

	m := cmux.New(ln)
	httpl := m.Match(cmux.HTTP1Fast())
	tlsl := m.Match(cmux.Any())

	if !s.AllowHTTP {
		s.httpserver.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Forwarded-Proto") == "https" && strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") {
				s.httpsserver.Handler.ServeHTTP(w, r)
				return
			}

			target := "https://" + r.Host + r.URL.Path
			if len(r.URL.RawQuery) > 0 {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		})
	}

	go func() {
		errChan <- errors.WithStack(s.httpserver.Serve(httpl))
	}()
	go func() {
		errChan <- errors.WithStack(s.httpsserver.ServeTLS(tlsl, "", ""))
	}()
	go func() {
		errChan <- errors.WithStack(m.Serve())
	}()

	return port, nil
}

// ProxyHandler wraps the regular handler to log the HTTP messages
func (s *Server) ProxyHandler(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	port := ""
	if strings.Contains(host, ":") {
		if lhost, lport, err := net.SplitHostPort(host); err == nil {
			host = lhost
			port = lport
		}
	}

	if port != "" {
		r.Host = net.JoinHostPort(host, port)
	} else {
		r.Host = host
	}

	pw := NewWriterProxy(w)
	s.Handler(pw, r)

	// push resources returned in Link headers from upstream middlewares or proxied apps
	resources, err := s.servePreloadLinks(w, r)
	if err != nil {
		s.Logger.Error().Msg(fmt.Sprintf("unable to preload links: %s", err.Error()))
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	status := pw.Response().StatusCode
	l := s.Logger.Info()
	if status >= 500 {
		l = s.Logger.Error()
	} else if status >= 400 {
		l = s.Logger.Warn()
	}
	l = l.Str("ip", ip).Int("status", status).Str("method", r.Method).Str("scheme", "https").Str("host", "127.0.0.1:8004")
	if len(resources) > 0 {
		l.Strs("preloaded_resources", resources)
	}
	l.Msg(r.RequestURI)
}

// Handler handles HTTP requests
func (s *Server) Handler(w http.ResponseWriter, r *http.Request) {
	// static file?
	if !strings.HasSuffix(strings.ToLower(r.URL.Path), ".php") {
		p := r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/") {
			p += "index.html"
		}
		path := path.Clean(filepath.Join(s.DocumentRoot, p))
		if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
			http.ServeFile(w, r, path)
			return
		}
	}

	if s.Callback == nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(html.WrapHTML("Page not found", html.CreateErrorTerminal("# Page not found"), "")))
		return
	}
	env := map[string]string{
		"SERVER_PORT":     s.serverPort,
		"SERVER_NAME":     r.Host,
		"SERVER_PROTOCOL": r.Proto,
		"SERVER_SOFTWARE": fmt.Sprintf("Symfony Local Server %s", s.Appversion),
	}
	env["X_FORWARDED_PORT"] = r.Header.Get("X-Forwarded-Port")
	if env["X_FORWARDED_PORT"] == "" {
		env["X_FORWARDED_PORT"] = s.serverPort
	}

	if err := s.Callback(w, r, env); err != nil {
		s.Logger.Error().Err(err).Msg("issue with server callback")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(html.WrapHTML(err.Error(), html.CreateErrorTerminal("# "+err.Error()), "")))
		return
	}
}
