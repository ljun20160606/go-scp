package scp

import (
	"context"
	"golang.org/x/crypto/ssh"
)

// SCP is the type for the SCP client.
type SCP struct {
	client *ssh.Client

	ctx context.Context

	sourceObserver SourceObserver
}

// NewSCP creates the SCP client.
// It is caller's responsibility to call Dial for ssh.Client before
// calling NewSCP and call Close for ssh.Client after using SCP.
func NewSCP(client *ssh.Client, options ...ScpOption) *SCP {
	s := &SCP{
		client: client,
		ctx:    context.Background(),
		sourceObserver: emptySourceObserver,
	}

	for _, option := range options {
		option(s)
	}
	return s
}

type ScpOption func(s *SCP)

func WithContext(ctx context.Context) ScpOption {
	return func(s *SCP) {
		s.ctx = ctx
	}
}

func WithSourceObserver(sourceObserver SourceObserver) ScpOption {
	return func(s *SCP) {
		s.sourceObserver = sourceObserver
	}
}
