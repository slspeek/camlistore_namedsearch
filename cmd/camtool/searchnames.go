/*
Copyright 2014 The Camlistore Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"camlistore.org/pkg/cmdmain"
	"camlistore.org/pkg/search"
)

type searchNamesCmd struct{}

func init() {
	cmdmain.RegisterCommand("searchnames", func(flags *flag.FlagSet) cmdmain.CommandRunner {
		return new(searchNamesCmd)
	})
}

func (c *searchNamesCmd) Describe() string {
	return "Gets and sets search aliases"
}

func (c *searchNamesCmd) Usage() {
	fmt.Fprintln(os.Stderr, "camtool searchnames <name> [substitute]")
}

func (c *searchNamesCmd) RunCommand(args []string) error {
	switch n := len(args); n {
	case 1:
		return c.getNamed(args[0])
	case 2:
		return c.setNamed(args[0], args[1])
	default:
		return cmdmain.UsageError("No more than two arguments allowed")
	}
	return nil
}

func (c *searchNamesCmd) getNamed(named string) error {
	cc := newClient("")
	gnr, err := cc.GetNamedSearch(&search.GetNamedRequest{Named: named})
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(gnr, "  ", "")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmdmain.Stdout, string(out))
	return nil
}

func (c *searchNamesCmd) setNamed(named, substitute string) error {
	cc := newClient("")
	snr, err := cc.SetNamedSearch(&search.SetNamedRequest{Named: named, Substitute: substitute})
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(snr, "  ", "")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmdmain.Stdout, string(out))
	return nil
}
