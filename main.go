// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("DoucheSync: %v", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Printf("DoucheSync %s\n", version)
		return nil
	case "keygen":
		fmt.Println(randomHex(32))
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	case "server", "client", "check-config":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		config := flags.String("config", configPath(), "TOML configuration file")
		once := flags.Bool("once", false, "client: scan and pull once, then exit")
		mode := flags.String("mode", "client", "check-config: client or server")
		if err := flags.Parse(args[1:]); err != nil {
			if err == flag.ErrHelp {
				return nil
			}
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q; put options after the command", flags.Arg(0))
		}
		command := args[0]
		if command == "check-config" {
			command = *mode
		}
		if command != "client" && command != "server" {
			return fmt.Errorf("mode must be client or server")
		}
		cfg, err := readConfig(*config, command)
		if err != nil {
			return err
		}
		if args[0] == "check-config" {
			fmt.Printf("Configuration valid (%s)\n", command)
			return nil
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if command == "server" {
			return runDiscovery(ctx, cfg.Server)
		}
		return runClient(ctx, cfg, *once)
	default:
		return fmt.Errorf("unknown command %q; use DoucheSync help", args[0])
	}
}
func usage() {
	fmt.Printf(`DoucheSync %s — direct peer-to-peer folder sync

Usage:
  DoucheSync server -config=server.toml
  DoucheSync client -config=client.toml [-once]
  DoucheSync check-config -config=client.toml [-mode=server]
  DoucheSync keygen
  DoucheSync version

Default config: %s
`, version, configPath())
}
