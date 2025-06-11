package cdk8sappproxy

import "github.com/go-logr/logr"

// gitProgressLogger buffers git progress messages and logs them line by line.
type gitProgressLogger struct {
	logger logr.Logger
	buffer []byte
}
