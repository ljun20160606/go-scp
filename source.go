package scp

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Receive copies a single remote file to the specified writer
// and returns the file information. The actual type of the file information is
// scp.FileInfo, and you can get the access time with fileInfo.(*scp.FileInfo).AccessTime().
func (s *SCP) Receive(srcFile string, dest io.Writer) (os.FileInfo, error) {
	var info os.FileInfo
	srcFile = realPath(filepath.Clean(srcFile))
	err := runResourceSession(s.ctx, s.client, srcFile, false, "", false, true, func(s *resourceSession) error {
		var timeHeader timeMsgHeader
		h, err := s.ReadHeaderOrReply()
		if err != nil {
			return fmt.Errorf("failed to read scp message header: err=%s", err)
		}
		var ok bool
		timeHeader, ok = h.(timeMsgHeader)
		if !ok {
			return fmt.Errorf("expected time message header, got %+v", h)
		}

		h, err = s.ReadHeaderOrReply()
		if err != nil {
			return fmt.Errorf("failed to read scp message header: err=%s", err)
		}
		fileHeader, ok := h.(fileMsgHeader)
		if !ok {
			return fmt.Errorf("expected file message header, got %+v", h)
		}
		if err := s.CopyFileBodyTo(fileHeader, dest); err != nil {
			return fmt.Errorf("failed to copy file: err=%s", err)
		}

		info = NewFileInfo(srcFile, fileHeader.Size, fileHeader.Mode, timeHeader.Mtime, timeHeader.Atime)
		return nil
	})
	return info, err
}

// ReceiveFile copies a single remote file to the local machine with
// the specified name. The time and permission will be set to the same value
// of the source file.
func (s *SCP) ReceiveFile(srcFile, destFile string) error {
	srcFile = realPath(filepath.Clean(srcFile))
	destFile = filepath.Clean(destFile)
	fiDest, err := os.Stat(destFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to get information of destnation file: err=%s", err)
	}
	if err == nil && fiDest.IsDir() {
		destFile = filepath.Join(destFile, filepath.Base(srcFile))
	}

	return runResourceSession(s.ctx, s.client, srcFile, false, "", false, true, func(s *resourceSession) error {
		h, err := s.ReadHeaderOrReply()
		if err != nil {
			return fmt.Errorf("failed to read scp message header: err=%s", err)
		}
		timeHeader, ok := h.(timeMsgHeader)
		if !ok {
			return fmt.Errorf("expected time message header, got %+v", h)
		}

		h, err = s.ReadHeaderOrReply()
		if err != nil {
			return fmt.Errorf("failed to read scp message header: err=%s", err)
		}
		fileHeader, ok := h.(fileMsgHeader)
		if !ok {
			return fmt.Errorf("expected file message header, got %+v", h)
		}

		return copyFileBodyFromRemote(s, destFile, timeHeader, fileHeader)
	})
}

func copyFileBodyFromRemote(s *resourceSession, localFilename string, timeHeader timeMsgHeader, fileHeader fileMsgHeader) error {
	file, err := os.OpenFile(localFilename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileHeader.Mode)
	if err != nil {
		return fmt.Errorf("failed to open destination file: err=%s", err)
	}

	if err := s.CopyFileBodyTo(fileHeader, file); err != nil {
		file.Close()
		return fmt.Errorf("failed to copy file: err=%s", err)
	}
	file.Close()

	if err := os.Chmod(localFilename, fileHeader.Mode); err != nil {
		return fmt.Errorf("failed to change file mode: err=%s", err)
	}

	if err := os.Chtimes(localFilename, timeHeader.Atime, timeHeader.Mtime); err != nil {
		return fmt.Errorf("failed to change file time: err=%s", err)
	}

	return nil
}

// ReceiveDir copies files and directories under a remote srcDir to
// to the destDir on the local machine. You can filter the files and directories
// to be copied with acceptFn. If acceptFn is nil, all files and directories will
// be copied. The time and permission will be set to the same value of the source
// file or directory.
func (s *SCP) ReceiveDir(srcDir, destDir string, acceptFn AcceptFunc) error {
	srcDir = realPath(filepath.Clean(srcDir))
	destDir = filepath.Clean(destDir)
	_, err := os.Stat(destDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to get information of destination directory: err=%s", err)
	}
	var skipsFirstDirectory bool
	if os.IsNotExist(err) {
		skipsFirstDirectory = true
		if err := os.MkdirAll(destDir, 0777); err != nil {
			return fmt.Errorf("failed to create destination directory: err=%s", err)
		}
	}

	if acceptFn == nil {
		acceptFn = acceptAny
	}

	return runResourceSession(s.ctx, s.client, srcDir, false, "", true, true, func(s *resourceSession) error {
		curDir := destDir
		var timeHeader timeMsgHeader
		var timeHeaders []timeMsgHeader
		isFirstStartDirectory := true
		var skipBaseDir string
		for {
			h, err := s.ReadHeaderOrReply()
			if err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("failed to read scp message header: err=%s", err)
			}
			switch h.(type) {
			case timeMsgHeader:
				timeHeader = h.(timeMsgHeader)
			case startDirectoryMsgHeader:
				dirHeader := h.(startDirectoryMsgHeader)

				if isFirstStartDirectory {
					isFirstStartDirectory = false
					if skipsFirstDirectory {
						continue
					}
				}

				curDir = filepath.Join(curDir, dirHeader.Name)
				timeHeaders = append(timeHeaders, timeHeader)

				if skipBaseDir != "" {
					continue
				}

				info := NewFileInfo(dirHeader.Name, 0, dirHeader.Mode|os.ModeDir, timeHeader.Mtime, timeHeader.Atime)
				accepted, err := acceptFn(filepath.Dir(curDir), info)
				if err != nil {
					return fmt.Errorf("error from accessFn: err=%s", err)
				}
				if !accepted {
					skipBaseDir = curDir
					continue
				}

				if err := os.MkdirAll(curDir, dirHeader.Mode); err != nil {
					return fmt.Errorf("failed to create directory: err=%s", err)
				}

				if err := os.Chmod(curDir, dirHeader.Mode); err != nil {
					return fmt.Errorf("failed to change directory mode: err=%s", err)
				}
			case endDirectoryMsgHeader:
				if len(timeHeaders) > 0 {
					timeHeader = timeHeaders[len(timeHeaders)-1]
					timeHeaders = timeHeaders[:len(timeHeaders)-1]
					if skipBaseDir == "" {
						if err := os.Chtimes(curDir, timeHeader.Atime, timeHeader.Mtime); err != nil {
							return fmt.Errorf("failed to change directory time: err=%s", err)
						}
					}
				}
				curDir = filepath.Dir(curDir)
				if skipBaseDir != "" {
					var sub bool
					if curDir == "" {
						sub = true
					} else {
						var err error
						sub, err = isSubdirectory(skipBaseDir, curDir)
						if err != nil {
							return fmt.Errorf("failed to check directory is subdirectory: err=%s", err)
						}
					}
					if !sub {
						skipBaseDir = ""
					}
				}
			case fileMsgHeader:
				fileHeader := h.(fileMsgHeader)
				if skipBaseDir == "" {
					info := NewFileInfo(fileHeader.Name, fileHeader.Size, fileHeader.Mode, timeHeader.Mtime, timeHeader.Atime)
					accepted, err := acceptFn(curDir, info)
					if err != nil {
						return fmt.Errorf("error from accessFn: err=%s", err)
					}
					if !accepted {
						continue
					}
					localFilename := filepath.Join(curDir, fileHeader.Name)
					if err = copyFileBodyFromRemote(s, localFilename, timeHeader, fileHeader); err != nil {
						return err
					}
				} else {
					if err := s.CopyFileBodyTo(fileHeader, ioutil.Discard); err != nil {
						return err
					}
				}
			case okMsg:
				// do nothing
			}
		}
		return nil
	})
}

func isSubdirectory(basepath, targetpath string) (bool, error) {
	rel, err := filepath.Rel(basepath, targetpath)
	if err != nil {
		return false, err
	}
	return !strings.HasPrefix(rel, ".."+string([]rune{filepath.Separator})), nil
}

type resourceSession struct {
	client            *ssh.Client
	session           *ssh.Session
	remoteSrcPath     string
	remoteSrcIsDir    bool
	scpPath           string
	recursive         bool
	updatesPermission bool
	stdin             io.WriteCloser
	stdout            io.Reader
	*resourceProtocol
}

func newResourceSession(client *ssh.Client, remoteSrcPath string, remoteSrcIsDir bool, scpPath string, recursive, updatesPermission bool) (*resourceSession, error) {
	s := &resourceSession{
		client:            client,
		remoteSrcPath:     remoteSrcPath,
		remoteSrcIsDir:    remoteSrcIsDir,
		scpPath:           scpPath,
		recursive:         recursive,
		updatesPermission: updatesPermission,
	}

	var err error
	s.session, err = s.client.NewSession()
	if err != nil {
		_ = s.session.Close()
		return nil, err
	}

	s.stdout, err = s.session.StdoutPipe()
	if err != nil {
		_ = s.session.Close()
		return nil, err
	}

	s.stdin, err = s.session.StdinPipe()
	if err != nil {
		_ = s.session.Close()
		return nil, err
	}

	if s.scpPath == "" {
		s.scpPath = "scp"
	}

	opt := []byte("-f")
	if s.updatesPermission {
		opt = append(opt, 'p')
	}
	if s.recursive {
		opt = append(opt, 'r')
	}
	if s.remoteSrcIsDir {
		opt = append(opt, 'd')
	}

	cmd := s.scpPath + " " + string(opt) + " " + escapeShellArg(s.remoteSrcPath)
	if err := s.session.Start(cmd); err != nil {
		_ = s.session.Close()
		return nil, err
	}

	s.resourceProtocol, err = newResourceProtocol(s.stdin, s.stdout)
	if err != nil {
		_ = s.session.Close()
		return nil, err
	}
	return s, nil
}

func (s *resourceSession) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close()
}

func (s *resourceSession) Wait() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Wait()
}

func runResourceSession(ctx context.Context, client *ssh.Client, remoteSrcPath string, remoteSrcIsDir bool, scpPath string, recursive, updatesPermission bool, handler func(s *resourceSession) error) error {
	s, err := newResourceSession(client, remoteSrcPath, remoteSrcIsDir, scpPath, recursive, updatesPermission)
	if err != nil {
		return err
	}
	defer s.Close()
	go func() {
		done := ctx.Done()
		// can never canceled
		if done == nil {
			return
		}
		select {
		case <-done:
			s.Close()
		}
	}()

	if err := handler(s); err != nil {
		return err
	}

	return s.Wait()
}
