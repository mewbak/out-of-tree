// Copyright 2018 Mikhail Klementev. All rights reserved.
// Use of this source code is governed by a GPLv3 license
// (or later) that can be found in the LICENSE file.

package qemukernel

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func readBytesUntilEOF(pipe io.ReadCloser) (buf []byte, err error) {
	bufSize := 1024
	for err != io.EOF {
		stdout := make([]byte, bufSize)
		var n int

		n, err = pipe.Read(stdout)
		if err != nil && err != io.EOF {
			return
		}

		buf = append(buf, stdout[:n]...)
	}

	if err == io.EOF {
		err = nil
	}
	return
}

func readUntilEOF(pipe io.ReadCloser) (str string, err error) {
	buf, err := readBytesUntilEOF(pipe)
	str = string(buf)
	return
}

type arch string

const (
	X86_64 arch = "x86_64"
	I386        = "i386"
	// TODO add other

	unsupported = "unsupported" // for test purposes
)

// Kernel describe kernel parameters for qemu
type Kernel struct {
	Name string
	Path string
}

// QemuSystem describe qemu parameters and runned process
type QemuSystem struct {
	arch      arch
	kernel    Kernel
	drivePath string

	Cpus   int
	Memory int

	// accessible while qemu is runned
	cmd  *exec.Cmd
	pipe struct {
		stdin  io.WriteCloser
		stderr io.ReadCloser
		stdout io.ReadCloser
	}
	died       bool
	sshHostFwd string

	// accessible after qemu is closed
	Stdout, Stderr string
	exitErr        error
}

// NewQemuSystem constructor
func NewQemuSystem(arch arch, kernel Kernel, drivePath string) (q QemuSystem, err error) {
	if _, err = exec.LookPath("qemu-system-" + string(arch)); err != nil {
		return
	}
	q.arch = arch

	if _, err = os.Stat(kernel.Path); err != nil {
		return
	}
	q.kernel = kernel

	if _, err = os.Stat(drivePath); err != nil {
		return
	}
	q.drivePath = drivePath

	// Default values
	q.Cpus = 2
	q.Memory = 512 // megabytes

	return
}

func getRandomAddrPort() (addr string) {
	// 127.1-255.0-255.0-255:10000-50000
	ip := fmt.Sprintf("127.%d.%d.%d",
		rand.Int()%254+1, rand.Int()%255, rand.Int()%254)
	port := rand.Int()%40000 + 10000
	return fmt.Sprintf("%s:%d", ip, port)
}

func getFreeAddrPort() (addrPort string) {
	for {
		addrPort = getRandomAddrPort()
		ln, err := net.Listen("tcp", addrPort)
		if err == nil {
			ln.Close()
			return
		}
	}
}

func kvmExists() bool {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return false
	}
	return true
}

// Start qemu process
func (q *QemuSystem) Start() (err error) {
	rand.Seed(time.Now().UnixNano()) // Are you sure?
	q.sshHostFwd = fmt.Sprintf("hostfwd=tcp:%s-:22", getFreeAddrPort())
	qemuArgs := []string{"-snapshot", "-nographic",
		"-hda", q.drivePath,
		"-kernel", q.kernel.Path,
		"-append", "root=/dev/sda console=ttyS0 rw",
		"-smp", fmt.Sprintf("%d", q.Cpus),
		"-m", fmt.Sprintf("%d", q.Memory),
		"-device", "e1000,netdev=n1",
		"-netdev", "user,id=n1," + q.sshHostFwd,
	}

	if (q.arch == X86_64 || q.arch == I386) && kvmExists() {
		qemuArgs = append(qemuArgs, "-enable-kvm")
	}

	q.cmd = exec.Command("qemu-system-"+string(q.arch), qemuArgs...)

	if q.pipe.stdin, err = q.cmd.StdinPipe(); err != nil {
		return
	}

	if q.pipe.stdout, err = q.cmd.StdoutPipe(); err != nil {
		return
	}

	if q.pipe.stderr, err = q.cmd.StderrPipe(); err != nil {
		return
	}

	err = q.cmd.Start()
	if err != nil {
		return
	}

	go func() {
		q.Stdout, _ = readUntilEOF(q.pipe.stdout)
		q.Stderr, _ = readUntilEOF(q.pipe.stderr)
		q.exitErr = q.cmd.Wait()
		q.died = true
	}()

	time.Sleep(time.Second / 10) // wait for immediately die

	if q.died {
		err = errors.New("qemu died immediately: " + q.Stderr)
	}

	return
}

// Stop qemu process
func (q *QemuSystem) Stop() {
	// 1  00/01   01  01  SOH  (Ctrl-A)  START OF HEADING
	fmt.Fprintf(q.pipe.stdin, "%cx", 1)
	// wait for die
	time.Sleep(time.Second / 10)
	if !q.died {
		q.cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(time.Second / 10)
		q.cmd.Process.Signal(syscall.SIGKILL)
	}
}
