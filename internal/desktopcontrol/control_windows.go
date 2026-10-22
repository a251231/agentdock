//go:build windows

package desktopcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var cancelSynchronousIOProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

func endpointPath(runtimeRoot string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(runtimeRoot)))
	return fmt.Sprintf(`\\.\pipe\agentdock-control-%x`, digest[:10])
}

func servePlatform(ctx context.Context, runtimeRoot string, handle func(context.Context, []byte) []byte) error {
	ctx, cancel := context.WithCancel(ctx)
	var connections sync.WaitGroup
	defer func() {
		cancel()
		connections.Wait()
	}()
	security, err := currentUserPipeSecurity()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(endpointPath(runtimeRoot))
	if err != nil {
		return err
	}
	for {
		pipe, err := windows.CreateNamedPipe(
			name,
			windows.PIPE_ACCESS_DUPLEX,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
			windows.PIPE_UNLIMITED_INSTANCES,
			maxMessageBytes+4,
			maxMessageBytes+4,
			0,
			security,
		)
		if err != nil {
			return fmt.Errorf("create desktop control named pipe: %w", err)
		}
		connectErr := connectNamedPipe(ctx, pipe)
		if ctx.Err() != nil {
			_ = windows.CloseHandle(pipe)
			return nil
		}
		if connectErr != nil {
			_ = windows.CloseHandle(pipe)
			return fmt.Errorf("connect desktop control named pipe: %w", connectErr)
		}
		// 单个客户端的半条请求或未读取的响应不能阻塞其他连接。
		connections.Add(1)
		go func() {
			defer connections.Done()
			servePipe(ctx, pipe, handle)
		}()
	}
}

func connectNamedPipe(ctx context.Context, pipe windows.Handle) error {
	return runPipeIO(ctx, func() error {
		err := windows.ConnectNamedPipe(pipe, nil)
		if errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return nil
		}
		return err
	})
}

// runPipeIO 让同步管道 I/O 服从上下文取消，包括连接、读取、写入和 flush。
func runPipeIO(ctx context.Context, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	type connectorState struct {
		thread windows.Handle
		err    error
	}

	ready := make(chan connectorState, 1)
	connected := make(chan error, 1)
	released := make(chan struct{})
	defer close(released)
	go func() {
		// CancelSynchronousIo 只会取消指定线程发起的同步 I/O。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// 调用方停止 CancelSynchronousIo 后才释放线程，避免误取消复用该线程的其他 I/O。
		defer func() { <-released }()

		thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
		ready <- connectorState{thread: thread, err: err}
		if err != nil {
			connected <- err
			return
		}

		if err := ctx.Err(); err != nil {
			connected <- err
			return
		}
		connected <- operation()
	}()

	connector := <-ready
	if connector.err != nil {
		return fmt.Errorf("open desktop control I/O thread: %w", connector.err)
	}
	defer windows.CloseHandle(connector.thread)

	select {
	case connectErr := <-connected:
		return connectErr
	case <-ctx.Done():
		// 取消可能先于实际 I/O 发起（ERROR_NOT_FOUND），也可能落在两次 I/O 之间。
		// 持续取消直到工作线程退出，之后调用方才可以安全关闭管道句柄。
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			_ = cancelSynchronousIO(connector.thread)
			select {
			case <-connected:
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}

func cancelSynchronousIO(thread windows.Handle) error {
	result, _, callErr := cancelSynchronousIOProc.Call(uintptr(thread))
	if result != 0 {
		return nil
	}
	if callErr == syscall.Errno(0) {
		return errors.New("CancelSynchronousIo failed without an error code")
	}
	return callErr
}

func servePipe(ctx context.Context, pipe windows.Handle, handle func(context.Context, []byte) []byte) {
	defer windows.CloseHandle(pipe)
	defer windows.DisconnectNamedPipe(pipe)
	ctx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancel()
	var request []byte
	err := runPipeIO(ctx, func() error {
		var err error
		request, err = readPipeMessage(pipe)
		return err
	})
	if err != nil {
		return
	}
	response := handle(ctx, request)
	_ = runPipeIO(ctx, func() error {
		if err := writePipeMessage(pipe, response); err != nil {
			return err
		}
		return windows.FlushFileBuffers(pipe)
	})
}

func callPlatform(ctx context.Context, runtimeRoot string, data []byte) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, controlRequestTimeout)
		defer cancel()
	}
	name, err := windows.UTF16PtrFromString(endpointPath(runtimeRoot))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	var pipe windows.Handle
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pipe, err = windows.CreateFile(
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			0,
			0,
		)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			return nil, fmt.Errorf("connect desktop control named pipe: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer windows.CloseHandle(pipe)
	var response []byte
	err = runPipeIO(ctx, func() error {
		if err := writePipeMessage(pipe, data); err != nil {
			return fmt.Errorf("write desktop control named pipe: %w", err)
		}
		var err error
		response, err = readPipeMessage(pipe)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read desktop control named pipe: %w", err)
	}
	return response, nil
}

func currentUserPipeSecurity() (*windows.SecurityAttributes, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open current process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current user token: %w", err)
	}
	// 仅当前用户和 LocalSystem 可以访问控制管道，避免同机其他用户修改运行配置。
	descriptor, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;%s)", user.User.Sid.String()),
	)
	if err != nil {
		return nil, fmt.Errorf("create desktop control security descriptor: %w", err)
	}
	attributes := &windows.SecurityAttributes{
		SecurityDescriptor: descriptor,
		InheritHandle:      0,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, nil
}

func writePipeMessage(pipe windows.Handle, data []byte) error {
	if len(data) > maxMessageBytes {
		return errors.New("desktop control message is too large")
	}
	buffer := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(buffer[:4], uint32(len(data)))
	copy(buffer[4:], data)
	for len(buffer) > 0 {
		var written uint32
		if err := windows.WriteFile(pipe, buffer, &written, nil); err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		buffer = buffer[written:]
	}
	return nil
}

func readPipeMessage(pipe windows.Handle) ([]byte, error) {
	header := make([]byte, 4)
	if err := readPipeFull(pipe, header); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(header)
	if size > maxMessageBytes {
		return nil, errors.New("desktop control message is too large")
	}
	data := make([]byte, size)
	if err := readPipeFull(pipe, data); err != nil {
		return nil, err
	}
	return data, nil
}

func readPipeFull(pipe windows.Handle, data []byte) error {
	for len(data) > 0 {
		var read uint32
		if err := windows.ReadFile(pipe, data, &read, nil); err != nil {
			return err
		}
		if read == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[read:]
	}
	return nil
}
