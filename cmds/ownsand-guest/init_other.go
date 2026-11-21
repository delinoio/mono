// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package main

import "log/slog"

func isInit() bool { return false }

func isInitChild() bool { return false }

func runInit([]string) int { return 1 }

func startTunnel(*slog.Logger) (<-chan error, error) { return nil, nil }
