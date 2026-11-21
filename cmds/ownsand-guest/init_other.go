// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import "log/slog"

func isInit() bool { return false }

func isInitChild() bool { return false }

func runInit([]string) int { return 1 }

func startTunnel(*slog.Logger) error { return nil }
