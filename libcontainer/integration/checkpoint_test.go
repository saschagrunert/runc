package integration

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/runc/libcontainer"
)

func showFile(t *testing.T, fname string) error {
	t.Logf("=== %s ===\n", fname)

	f, err := os.Open(fname)
	if err != nil {
		t.Log(err)
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		t.Log(scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	t.Logf("=== END ===\n")

	return nil
}

func TestUsernsCheckpoint(t *testing.T) {
	if _, err := os.Stat("/proc/self/ns/user"); os.IsNotExist(err) {
		t.Skip("userns is unsupported")
	}
	cmd := exec.Command("criu", "check", "--feature", "userns")
	if err := cmd.Run(); err != nil {
		t.Skip("Unable to c/r a container with userns")
	}
	testCheckpoint(t, true)
}

func TestCheckpoint(t *testing.T) {
	testCheckpoint(t, false)
}

func testCheckpoint(t *testing.T, userns bool) {
	if testing.Short() {
		return
	}

	if _, err := exec.LookPath("criu"); err != nil {
		t.Skipf("criu binary not found: %v", err)
	}

	root, err := newTestRoot()
	ok(t, err)
	defer os.RemoveAll(root)

	rootfs, err := newRootfs()
	ok(t, err)
	defer remove(rootfs)

	config := newTemplateConfig(t, &tParam{
		rootfs: rootfs,
		userns: userns,
	})
	factory, err := libcontainer.New(root, libcontainer.Cgroupfs)
	ok(t, err)

	container, err := factory.Create("test", config)
	ok(t, err)
	defer container.Destroy()

	stdinR, stdinW, err := os.Pipe()
	ok(t, err)

	var stdout bytes.Buffer

	pconfig := libcontainer.Process{
		Cwd:    "/",
		Args:   []string{"cat"},
		Env:    standardEnvironment,
		Stdin:  stdinR,
		Stdout: &stdout,
		Init:   true,
	}

	err = container.Run(&pconfig)
	stdinR.Close()
	defer stdinW.Close()
	ok(t, err)

	pid, err := pconfig.Pid()
	ok(t, err)

	process, err := os.FindProcess(pid)
	ok(t, err)

	parentDir, err := ioutil.TempDir("", "criu-parent")
	ok(t, err)
	defer os.RemoveAll(parentDir)

	preDumpOpts := &libcontainer.CriuOpts{
		ImagesDirectory: parentDir,
		WorkDirectory:   parentDir,
		PreDump:         true,
	}
	preDumpLog := filepath.Join(preDumpOpts.WorkDirectory, "dump.log")

	if err := container.Checkpoint(preDumpOpts); err != nil {
		showFile(t, preDumpLog)
		t.Fatal(err)
	}

	state, err := container.Status()
	ok(t, err)

	if state != libcontainer.Running {
		t.Fatal("Unexpected preDump state: ", state)
	}

	imagesDir, err := ioutil.TempDir("", "criu")
	ok(t, err)
	defer os.RemoveAll(imagesDir)

	checkpointOpts := &libcontainer.CriuOpts{
		ImagesDirectory: imagesDir,
		WorkDirectory:   imagesDir,
		ParentImage:     "../criu-parent",
	}
	dumpLog := filepath.Join(checkpointOpts.WorkDirectory, "dump.log")
	restoreLog := filepath.Join(checkpointOpts.WorkDirectory, "restore.log")

	if err := container.Checkpoint(checkpointOpts); err != nil {
		showFile(t, dumpLog)
		t.Fatal(err)
	}

	state, err = container.Status()
	ok(t, err)

	if state != libcontainer.Stopped {
		t.Fatal("Unexpected state checkpoint: ", state)
	}

	stdinW.Close()
	_, err = process.Wait()
	ok(t, err)

	// reload the container
	container, err = factory.Load("test")
	ok(t, err)

	restoreStdinR, restoreStdinW, err := os.Pipe()
	ok(t, err)

	var restoreStdout bytes.Buffer
	restoreProcessConfig := &libcontainer.Process{
		Cwd:    "/",
		Stdin:  restoreStdinR,
		Stdout: &restoreStdout,
		Init:   true,
	}

	err = container.Restore(restoreProcessConfig, checkpointOpts)
	restoreStdinR.Close()
	defer restoreStdinW.Close()
	if err != nil {
		showFile(t, restoreLog)
		t.Fatal(err)
	}

	state, err = container.Status()
	ok(t, err)
	if state != libcontainer.Running {
		t.Fatal("Unexpected restore state: ", state)
	}

	pid, err = restoreProcessConfig.Pid()
	ok(t, err)

	_, err = os.FindProcess(pid)
	ok(t, err)

	_, err = restoreStdinW.WriteString("Hello!")
	ok(t, err)

	restoreStdinW.Close()
	s, err := restoreProcessConfig.Wait()
	ok(t, err)

	if !s.Success() {
		t.Fatal(s.String(), pid)
	}

	output := restoreStdout.String()
	if !strings.Contains(output, "Hello!") {
		t.Fatal("Did not restore the pipe correctly:", output)
	}
}
