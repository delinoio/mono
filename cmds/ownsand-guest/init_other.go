// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

func isInit() bool { return false }

func runInit([]string) int { return 1 }
