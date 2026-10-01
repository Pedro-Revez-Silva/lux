package main

import (
	"log/slog"

	"github.com/marcioapm/lux/internal/ec2"
	"github.com/marcioapm/lux/internal/server"
)

// providers builds the host provisioners luxd can use: EC2, with the
// standard AWS configuration (ec2.endpoint overrides the endpoint).
func providers(c config, log *slog.Logger) map[string]server.Provider {
	return map[string]server.Provider{"ec2": ec2.New(c.EC2.Endpoint, log)}
}
