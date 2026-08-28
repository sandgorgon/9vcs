package main

import (
	"fmt"

	"github.com/sandgorgon/9auth"
)

func cmdIdentity(args []string) error {
	if len(args) != 1 || args[0] != "show" {
		return fmt.Errorf("identity: usage: 9vcs identity show")
	}
	id, err := auth.Load()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	fmt.Println(id.Fingerprint())
	return nil
}
