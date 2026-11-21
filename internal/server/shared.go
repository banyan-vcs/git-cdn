package server

import "gitlab.com/banyan-vcs/git-cdn/internal/nativegit"

// NativeGit shares the process budget with SSH hydration and pack generation.
func (s *Server) NativeGit() *nativegit.Runner { return s.git }

func (s *Server) RecordSSHPassthrough() { s.proxied.Add(1) }
