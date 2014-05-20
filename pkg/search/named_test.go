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

package search

import (
	"bytes"
	"camlistore.org/pkg/test"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSetNamed(t *testing.T) {
	test.BrokenTest(t)
	w := test.GetWorld(t)

	t.Log(w.Addr())
	setCmd := w.Cmd("camtool", "searchnames", "foo", "is:pano")
	sno, err := test.RunCmd(setCmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Server responded to setname with: %v", sno)

	time.Sleep(1 * time.Second)
	setCmd = w.Cmd("camtool", "searchnames", "bar", "is:image")
	sno, err = test.RunCmd(setCmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Server responded to setname with: %v", sno)
	getCmd := w.Cmd("camtool", "searchnames", "foo")
	gno, err := test.RunCmd(getCmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Returned from getnamed: %s", gno)

	var gnr GetNamedResponse
	err = json.Unmarshal(bytes.NewBufferString(gno).Bytes(), &gnr)
	if err != nil {
		t.Fatal(err)
	}
	if gnr.Named != "foo" || gnr.Substitute != "is:pano" {
		t.Errorf("Unexpected value %v , expected (foo, is:pano)", gnr)
	}
}

func TestGetNamed(t *testing.T) {
	w := test.GetWorld(t)

	putExprCmd := w.Cmd("camput", "blob", "-")
	putExprCmd.Stdin = strings.NewReader("is:pano")
	ref, err := test.RunCmd(putExprCmd)
	if err != nil {
		t.Fatal(err)
	}

	permanodeCmd := w.Cmd("camput", "permanode")
	pn, err := test.RunCmd(permanodeCmd)
	if err != nil {
		t.Fatal(err)
	}

	setNamedCmd := w.Cmd("camput", "attr", strings.TrimSpace(pn), "camliNamedSearch", "foo")
	_, err = test.RunCmd(setNamedCmd)
	if err != nil {
		t.Fatal(err)
	}

	setConCmd := w.Cmd("camput", "attr", strings.TrimSpace(pn), "camliContent", strings.TrimSpace(ref))
	_, err = test.RunCmd(setConCmd)
	if err != nil {
		t.Fatal(err)
	}

	getCmd := w.Cmd("camtool", "searchnames", "foo")
	gno, err := test.RunCmd(getCmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Returned from getnamed: %s", gno)

	var gnr GetNamedResponse
	err = json.Unmarshal(bytes.NewBufferString(gno).Bytes(), &gnr)
	if err != nil {
		t.Fatal(err)
	}
	if gnr.Named != "foo" || gnr.Substitute != "is:pano" {
		t.Errorf("Unexpected value %v , expected (foo, is:pano)", gnr)
	}
}
