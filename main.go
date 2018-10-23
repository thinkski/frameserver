// Serves latest V4L2 device frame via simple HTTP server
// Copyright 2018 Chris Hiszpanski. All rights reserved.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var flagHttpPort int
var flagVideoIn string

func init() {
	flag.StringVar(&flagVideoIn, "i", "/dev/video0", "Input device")
	flag.IntVar(&flagHttpPort, "p", 8000, "Server listens on this port")
}

const (
	V4L2_BUF_TYPE_VIDEO_CAPTURE = 1
	V4L2_PIX_FMT_JPEG           = 0x4745504a
	V4L2_FIELD_NONE             = 1
	V4L2_MEMORY_MMAP            = 1
	VIDIOC_S_FMT                = 0xc0cc5605
	VIDIOC_REQBUFS              = 0xc0145608
	VIDIOC_QUERYBUF             = 0xc0445609
	VIDIOC_STREAMON             = 0x40045612
	VIDIOC_STREAMOFF            = 0x40045613
	VIDIOC_QBUF                 = 0xc044560f
	VIDIOC_DQBUF                = 0xc0445611
	VIDIOC_QUERYCAP             = 0x80685600
)

type v4l2_capability struct {
	driver       [16]uint8
	card         [32]uint8
	bus_info     [32]uint8
	version      uint32
	capabilities uint32
	device_caps  uint32
	reserved     [3]uint32
}

type v4l2_pix_format struct {
	typ          uint32
	width        uint32
	height       uint32
	pixelformat  uint32
	field        uint32
	bytesperline uint32
	sizeimage    uint32
	colorspace   uint32
	priv         uint32
}

type v4l2_requestbuffers struct {
	count    uint32
	typ      uint32
	memory   uint32
	reserved [2]uint32
}

type v4l2_timecode struct {
	typ      uint32
	flags    uint32
	frames   uint8
	seconds  uint8
	minutes  uint8
	hours    uint8
	userbits [4]uint8
}

type timeval struct {
	tv_sec  uint32
	tv_usec uint32
}

type v4l2_buffer struct {
	index     uint32
	typ       uint32
	bytesused uint32
	flags     uint32
	field     uint32
	timestamp timeval
	timecode  v4l2_timecode
	sequence  uint32
	memory    uint32
	offset    uint32
	length    uint32
	reserved2 uint32
	reserved  uint32
}

func uint8_to_string(bs []uint8) string {
	ba := make([]byte, 0, len(bs))
	for _, b := range bs {
		ba = append(ba, byte(b))
	}
	return string(ba)
}

// Ensures multiple readers and one writer use shared memory harmoniously
var mutex sync.RWMutex
var jpegLen uint32

// getJPEG returns an http handler which returns a JPEG from the specified
// memory mapped data buffer
func getJPEG(data []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Lock for reading (writer may not write at this time)
		mutex.RLock()
		defer mutex.RUnlock()

		w.Header().Set("Cache-Control", "no-store")
		w.Write(data[:jpegLen])
	})
}

// framePump continuously dequeues and re-enqueues buffers. Ensures that
// when getImage is called, the dequeued buffer is the latest available.
func framePump(fd int) error {
	// File descriptor set
	fds := unix.FdSet{}

	// Set bit in set corresponding to file descriptor
	fds.Bits[fd>>8] |= 1 << (uint(fd) & 63)

	for {
		// Wait for frame
		_, err := unix.Select(fd+1, &fds, nil, nil, nil)
		if err != nil {
			return err
		}

		// Lock for writing
		mutex.Lock()

		// Dequeue buffer
		qbuf := v4l2_buffer{
			typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
			memory: V4L2_MEMORY_MMAP,
			index:  0,
		}
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(fd),
			uintptr(VIDIOC_DQBUF),
			uintptr(unsafe.Pointer(&qbuf)),
		)
		if errno != 0 {
			mutex.Unlock()
			return errno
		}

		// Save buffer size
		jpegLen = qbuf.bytesused

		// Unlock for readers
		mutex.Unlock()

		// Enqueue buffer
		_, _, errno = syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(fd),
			uintptr(VIDIOC_QBUF),
			uintptr(unsafe.Pointer(&qbuf)),
		)
		if errno != 0 {
			return errno
		}
	}
}

func main() {
	flag.Parse()

	// Open video device
	dev, err := unix.Open(flagVideoIn, unix.O_RDWR|unix.O_NONBLOCK, 0666)
	if err != nil {
		log.Fatal("Open: ", err)
	}
	defer unix.Close(dev)

	// Set format
	pfmt := v4l2_pix_format{
		typ:         V4L2_BUF_TYPE_VIDEO_CAPTURE,
		width:       1280,
		height:      960,
		pixelformat: V4L2_PIX_FMT_JPEG,
		field:       V4L2_FIELD_NONE,
	}
	_, _, errno := unix.Syscall(
		syscall.SYS_IOCTL,
		uintptr(dev),
		uintptr(VIDIOC_S_FMT),
		uintptr(unsafe.Pointer(&pfmt)),
	)
	if errno != 0 {
		log.Fatal("Set format: ", errno)
	}

	// Request buffer
	req := v4l2_requestbuffers{
		count:  1,
		typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
		memory: V4L2_MEMORY_MMAP,
	}
	_, _, errno = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(dev),
		uintptr(VIDIOC_REQBUFS),
		uintptr(unsafe.Pointer(&req)),
	)
	if errno != 0 {
		log.Fatal("Request buffer: ", errno)
	}

	// Query buffer parameters (namely memory offset and length)
	buf := v4l2_buffer{
		typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
		memory: V4L2_MEMORY_MMAP,
		index:  0,
	}
	_, _, errno = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(dev),
		uintptr(VIDIOC_QUERYBUF),
		uintptr(unsafe.Pointer(&buf)),
	)
	if errno != 0 {
		log.Fatal("Query buffer: ", errno)
	}

	// Map memory
	data, err := unix.Mmap(
		dev,
		int64(buf.offset),
		int(buf.length),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED,
	)
	if err != nil {
		log.Fatal(err)
	}
	defer unix.Munmap(data)

	// Enqueue an initial buffer
	qbuf := v4l2_buffer{
		typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
		memory: V4L2_MEMORY_MMAP,
		index:  0,
	}
	_, _, errno = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(dev),
		uintptr(VIDIOC_QBUF),
		uintptr(unsafe.Pointer(&qbuf)),
	)
	if errno != 0 {
		log.Fatal("qbuf: ", errno)
	}

	// Start stream
	_, _, errno = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(dev),
		uintptr(VIDIOC_STREAMON),
		uintptr(unsafe.Pointer(&buf.typ)),
	)
	if errno != 0 {
		log.Fatal("Start stream: ", errno)
	}

	// Driver has additional internal buffer. Drain it to keep it fresh.
	go framePump(dev)

	// Stop stream (on shutdown)
	defer func(fd int, typ *uint32) {
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(fd),
			uintptr(VIDIOC_STREAMOFF),
			uintptr(unsafe.Pointer(typ)),
		)
		if errno != 0 {
			log.Fatal("Start stream: ", errno)
		}
	}(dev, &buf.typ)

	// Start web server
	http.Handle("/image.jpg", getJPEG(data))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", flagHttpPort), nil))
}
