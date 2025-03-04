//go:build !nohdfs
// +build !nohdfs

/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package object

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/colinmarc/hdfs/v2"
	"github.com/colinmarc/hdfs/v2/hadoopconf"
)

var superuser = "hdfs"
var supergroup = "supergroup"

type hdfsclient struct {
	DefaultObjectStorage
	addr           string
	c              *hdfs.Client
	dfsReplication int
	basePath       string
}

func (h *hdfsclient) String() string {
	return fmt.Sprintf("hdfs://%s%s", h.addr, h.basePath)
}

func (h *hdfsclient) path(key string) string {
	return h.basePath + key
}

func (h *hdfsclient) Head(key string) (Object, error) {
	info, err := h.c.Stat(h.path(key))
	if err != nil {
		return nil, err
	}

	return h.toFile(key, info), nil
}

func (h *hdfsclient) toFile(key string, info os.FileInfo) *file {
	hinfo := info.(*hdfs.FileInfo)
	f := &file{
		obj{
			key,
			info.Size(),
			info.ModTime(),
			info.IsDir(),
			"",
		},
		hinfo.Owner(),
		hinfo.OwnerGroup(),
		info.Mode(),
		false,
	}
	if f.owner == superuser {
		f.owner = "root"
	}
	if f.group == supergroup {
		f.group = "root"
	}
	// stickybit from HDFS is different than golang
	if f.mode&01000 != 0 {
		f.mode &= ^os.FileMode(01000)
		f.mode |= os.ModeSticky
	}
	if info.IsDir() {
		f.size = 0
		if !strings.HasSuffix(f.key, "/") && f.key != "" {
			f.key += "/"
		}
	}
	return f
}

func (h *hdfsclient) Get(key string, off, limit int64) (io.ReadCloser, error) {
	f, err := h.c.Open(h.path(key))
	if err != nil {
		return nil, err
	}

	finfo := f.Stat()
	if finfo.IsDir() {
		return io.NopCloser(bytes.NewBuffer([]byte{})), nil
	}

	if limit > 0 {
		return &SectionReaderCloser{
			SectionReader: io.NewSectionReader(f, off, limit),
			Closer:        f,
		}, nil
	}
	return f, nil
}

const abcException = "org.apache.hadoop.hdfs.protocol.AlreadyBeingCreatedException"

func (h *hdfsclient) Put(key string, in io.Reader) error {
	path := h.path(key)
	if strings.HasSuffix(path, dirSuffix) {
		return h.c.MkdirAll(path, os.FileMode(0755))
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.tmp.%d", filepath.Base(path), rand.Int()))
	f, err := h.c.CreateFile(tmp, h.dfsReplication, 128<<20, 0755)
	defer func() { _ = h.c.Remove(tmp) }()
	if err != nil {
		if pe, ok := err.(*os.PathError); ok && pe.Err == os.ErrNotExist {
			_ = h.c.MkdirAll(filepath.Dir(path), 0755)
			f, err = h.c.CreateFile(tmp, h.dfsReplication, 128<<20, 0755)
		}
		if pe, ok := err.(*os.PathError); ok {
			if remoteErr, ok := pe.Err.(hdfs.Error); ok && remoteErr.Exception() == abcException {
				pe.Err = os.ErrExist
			}
			if pe.Err == os.ErrExist {
				_ = h.c.Remove(tmp)
				f, err = h.c.CreateFile(tmp, h.dfsReplication, 128<<20, 0755)
			}
		}
		if err != nil {
			return err
		}
	}
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	_, err = io.CopyBuffer(f, in, *buf)
	if err != nil {
		_ = f.Close()
		return err
	}
	err = f.Close()
	if err != nil && !IsErrReplicating(err) {
		return err
	}
	return h.c.Rename(tmp, path)
}

func IsErrReplicating(err error) bool {
	pe, ok := err.(*os.PathError)
	return ok && pe.Err == hdfs.ErrReplicating
}

func (h *hdfsclient) Delete(key string) error {
	err := h.c.Remove(h.path(key))
	if err != nil && os.IsNotExist(err) {
		err = nil
	}
	return err
}

func (h *hdfsclient) List(prefix, marker, delimiter string, limit int64) ([]Object, error) {
	if delimiter != "/" {
		return nil, notSupported
	}
	dir := h.path(prefix)
	var objs []Object
	if !strings.HasSuffix(dir, "/") {
		dir = filepath.Dir(dir) + dirSuffix
	} else if marker == "" {
		obj, err := h.Head(prefix)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		objs = append(objs, obj)
	}

	file, err := h.c.Open(dir)
	var entries []os.FileInfo
	if file != nil {
		entries, err = file.Readdir(0)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// make sure they are ordered in full path
	entryMap := make(map[string]fs.FileInfo)
	names := make([]string, len(entries))
	for i, info := range entries {
		if info.IsDir() {
			names[i] = info.Name() + "/"
		} else {
			names[i] = info.Name()
		}
		entryMap[names[i]] = info
	}
	sort.Strings(names)

	for _, name := range names {
		p := dir + name
		if !strings.HasPrefix(p, h.basePath) {
			continue
		}
		key := p[len(h.basePath):]
		if !strings.HasPrefix(key, prefix) || (marker != "" && key <= marker) {
			continue
		}
		f := h.toFile(key, entryMap[name])
		objs = append(objs, f)
		if len(objs) >= int(limit) {
			break
		}
	}
	return objs, nil
}

func (h *hdfsclient) walk(path string, walkFn filepath.WalkFunc) error {
	file, err := h.c.Open(path)
	var info os.FileInfo
	if file != nil {
		info = file.Stat()
	}

	err = walkFn(path, info, err)
	if err != nil {
		if info != nil && info.IsDir() && err == filepath.SkipDir {
			return nil
		}

		return err
	}

	if info == nil || !info.IsDir() {
		return nil
	}

	infos, err := file.Readdir(0)
	if err != nil {
		return walkFn(path, info, err)
	}

	// make sure they are ordered in full path
	names := make([]string, len(infos))
	for i, info := range infos {
		if info.IsDir() {
			names[i] = info.Name() + "/"
		} else {
			names[i] = info.Name()
		}
	}
	sort.Strings(names)

	for _, name := range names {
		name = strings.TrimSuffix(name, "/")
		err = h.walk(filepath.ToSlash(filepath.Join(path, name)), walkFn)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *hdfsclient) ListAll(prefix, marker string) (<-chan Object, error) {
	listed := make(chan Object, 10240)
	root := h.path(prefix)
	_, err := h.c.Stat(root)
	if err != nil && err.(*os.PathError).Err == os.ErrNotExist || !strings.HasSuffix(prefix, "/") {
		root = filepath.Dir(root)
	}
	_, err = h.c.Stat(root)
	if err != nil && err.(*os.PathError).Err == os.ErrNotExist {
		close(listed)
		return listed, nil // return empty list
	}
	go func() {
		_ = h.walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if err == io.EOF {
					err = nil // ignore
				} else {
					logger.Errorf("list %s: %s", path, err)
					listed <- nil
				}
				return err
			}
			key := path[len(h.basePath):]
			if !strings.HasPrefix(key, prefix) || (marker != "" && key <= marker) {
				if info.IsDir() && !strings.HasPrefix(prefix, key) && !strings.HasPrefix(marker, key) {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() {
				if path != root || !strings.HasSuffix(root, "/") {
					key += "/"
				}
			}
			f := h.toFile(key, info)
			listed <- f
			return nil
		})
		close(listed)
	}()
	return listed, nil
}

func (h *hdfsclient) Chtimes(key string, mtime time.Time) error {
	return h.c.Chtimes(h.path(key), mtime, mtime)
}

func (h *hdfsclient) Chmod(key string, mode os.FileMode) error {
	return h.c.Chmod(h.path(key), mode)
}

func (h *hdfsclient) Chown(key string, owner, group string) error {
	if owner == "root" {
		owner = superuser
	}
	if group == "root" {
		group = supergroup
	}
	return h.c.Chown(h.path(key), owner, group)
}

func newHDFS(addr, username, sk, token string) (ObjectStorage, error) {
	conf, err := hadoopconf.LoadFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("Problem loading configuration: %s", err)
	}

	rpcAddr, basePath := parseHDFSAddr(addr, conf)
	options := hdfs.ClientOptionsFromConf(conf)
	if addr != "" {
		options.Addresses = rpcAddr
		logger.Infof("HDFS Addresses: %s, basePath: %s", rpcAddr, basePath)
	}

	if options.KerberosClient != nil {
		options.KerberosClient, err = getKerberosClient()
		if err != nil {
			return nil, fmt.Errorf("Problem with kerberos authentication: %s", err)
		}
	} else {
		if username == "" {
			username = os.Getenv("HADOOP_USER_NAME")
		}
		if username == "" {
			current, err := user.Current()
			if err != nil {
				return nil, fmt.Errorf("get current user: %s", err)
			}
			username = current.Username
		}
		options.User = username
	}

	c, err := hdfs.NewClient(options)
	if err != nil {
		return nil, fmt.Errorf("new HDFS client %s: %s", rpcAddr, err)
	}
	if os.Getenv("HADOOP_SUPER_USER") != "" {
		superuser = os.Getenv("HADOOP_SUPER_USER")
	}
	if os.Getenv("HADOOP_SUPER_GROUP") != "" {
		supergroup = os.Getenv("HADOOP_SUPER_GROUP")
	}

	var replication = 3
	if replication_conf, found := conf["dfs.replication"]; found {
		if x, err := strconv.Atoi(replication_conf); err == nil {
			replication = x
		}
	}

	return &hdfsclient{addr: strings.Join(rpcAddr, ","), c: c, dfsReplication: replication, basePath: basePath}, nil
}

// addr can be hdfs://nameservice e.g. hdfs://example, hdfs://example/user/juicefs
// convert the nameservice as a comma separated list of host:port by referencing hadoop conf
func parseHDFSAddr(addr string, conf hadoopconf.HadoopConf) (rpcAddresses []string, basePath string) {
	addr = strings.TrimPrefix(addr, "hdfs://")
	sp := strings.SplitN(addr, "/", 2)
	authority := sp[0]

	// check if it is a nameservice
	var nns []string
	confParam := "dfs.namenode.rpc-address." + authority
	for key, value := range conf {
		if key == confParam || strings.HasPrefix(key, confParam+".") {
			nns = append(nns, value)
		}
	}
	if len(nns) > 0 {
		rpcAddresses = nns
	} else {
		rpcAddresses = strings.Split(authority, ",")
	}
	basePath = "/"
	if len(sp) > 1 && len(sp[1]) > 0 {
		basePath += strings.TrimRight(sp[1], "/") + "/"
	}
	return
}

func init() {
	Register("hdfs", newHDFS)
}
