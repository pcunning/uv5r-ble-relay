package main

import "syscall"

// syscallNoctty returns the platform's O_NOCTTY flag (0 if unavailable).
func syscallNoctty() int { return syscall.O_NOCTTY }
