package fuse

// This file contains the internal logic of the
// FileSystemConnector. The functions for satisfying the raw interface are in
// fsops.go

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

// Tests should set to true.
var paranoia = false

func NewFileSystemOptions() *FileSystemOptions {
	return &FileSystemOptions{
		NegativeTimeout: 0.0,
		AttrTimeout:     1.0,
		EntryTimeout:    1.0,
		Owner:           CurrentOwner(),
	}
}

// FilesystemConnector is a raw FUSE filesystem that manages in-process mounts and inodes.
type FileSystemConnector struct {
	DefaultRawFileSystem

	Debug bool

	fsInit   RawFsInit
	inodeMap HandleMap
	rootNode *Inode
}

func NewFileSystemConnector(nodeFs NodeFileSystem, opts *FileSystemOptions) (me *FileSystemConnector) {
	me = new(FileSystemConnector)
	if opts == nil {
		opts = NewFileSystemOptions()
	}
	me.inodeMap = NewHandleMap(!opts.SkipCheckHandles)
	me.rootNode = me.newInode(true)
	me.rootNode.nodeId = FUSE_ROOT_ID
	me.verify()
	me.mountRoot(nodeFs, opts)
	return me
}

func (me *FileSystemConnector) verify() {
	if !paranoia {
		return
	}
	root := me.rootNode
	root.verify(me.rootNode.mountPoint)
}

func (me *FileSystemConnector) newInode(isDir bool) *Inode {
	data := new(Inode)
	data.nodeId = me.inodeMap.Register(&data.handled)
	data.connector = me
	if isDir {
		data.children = make(map[string]*Inode, initDirSize)
	}

	return data
}

func (me *FileSystemConnector) createChild(parent *Inode, name string, fi *os.FileInfo, fsi FsNode) (out *EntryOut, child *Inode) {
	child = parent.CreateChild(name, fi.IsDirectory(), fsi)
	out = parent.mount.fileInfoToEntry(fi)
	out.Ino = child.nodeId
	out.NodeId = child.nodeId
	return out, child
}

func (me *FileSystemConnector) lookupUpdate(parent *Inode, name string, isDir bool, lookupCount int) *Inode {
	defer me.verify()

	parent.treeLock.Lock()
	defer parent.treeLock.Unlock()

	data, ok := parent.children[name]
	if !ok {
		data = me.newInode(isDir)
		parent.addChild(name, data)
		data.mount = parent.mount
		data.treeLock = &data.mount.treeLock
	}
	data.lookupCount += lookupCount
	return data
}

func (me *FileSystemConnector) lookupMount(parent *Inode, name string, lookupCount int) (mount *fileSystemMount) {
	parent.treeLock.RLock()
	defer parent.treeLock.RUnlock()
	if parent.mounts == nil {
		return nil
	}

	mount, ok := parent.mounts[name]
	if ok {
		mount.treeLock.Lock()
		defer mount.treeLock.Unlock()
		mount.mountInode.lookupCount += lookupCount
		return mount
	}
	return nil
}

func (me *FileSystemConnector) getInodeData(nodeid uint64) *Inode {
	if nodeid == FUSE_ROOT_ID {
		return me.rootNode
	}
	return (*Inode)(unsafe.Pointer(DecodeHandle(nodeid)))
}

func (me *FileSystemConnector) forgetUpdate(nodeId uint64, forgetCount int) {
	defer me.verify()

	node := me.getInodeData(nodeId)

	node.treeLock.Lock()
	defer node.treeLock.Unlock()

	node.lookupCount -= forgetCount
	me.considerDropInode(node)
}

func (me *FileSystemConnector) considerDropInode(n *Inode) (drop bool) {
	delChildren := []string{}
	for k, v := range n.children {
		if v.mountPoint == nil && me.considerDropInode(v) {
			delChildren = append(delChildren, k)
		}
	}
	for _, k := range delChildren {
		ch := n.rmChild(k)
		if ch == nil {
			panic(fmt.Sprintf("trying to del child %q, but not present", k))
		}
		me.inodeMap.Forget(ch.nodeId)
	}

	if len(n.children) > 0 || n.lookupCount > 0 {
		return false
	}
	if n == me.rootNode || n.mountPoint != nil {
		return false
	}

	n.openFilesMutex.Lock()
	defer n.openFilesMutex.Unlock()
	return len(n.openFiles) == 0
}

func (me *FileSystemConnector) renameUpdate(oldParent *Inode, oldName string, newParent *Inode, newName string) {
	defer me.verify()
	oldParent.treeLock.Lock()
	defer oldParent.treeLock.Unlock()

	if oldParent.mount != newParent.mount {
		panic("Cross mount rename")
	}

	node := oldParent.rmChild(oldName)
	if node == nil {
		panic("Source of rename does not exist")
	}
	newParent.rmChild(newName)
	newParent.addChild(newName, node)
}

func (me *FileSystemConnector) unlinkUpdate(parent *Inode, name string) {
	defer me.verify()

	parent.treeLock.Lock()
	defer parent.treeLock.Unlock()

	parent.rmChild(name)
}

// Walk the file system starting from the root. Will return nil if
// node not found.
func (me *FileSystemConnector) findLastKnownInode(fullPath string) (*Inode, []string) {
	if fullPath == "" {
		return me.rootNode, nil
	}

	fullPath = strings.TrimLeft(filepath.Clean(fullPath), "/")
	comps := strings.Split(fullPath, "/")

	node := me.rootNode
	for i, component := range comps {
		if len(component) == 0 {
			continue
		}

		if node.mountPoint != nil {
			node.mountPoint.treeLock.RLock()
			defer node.mountPoint.treeLock.RUnlock()
		}

		next := node.children[component]
		if next == nil {
			return node, comps[i:]
		}
		node = next
	}

	return node, nil
}

func (me *FileSystemConnector) findInode(fullPath string) *Inode {
	n, rest := me.findLastKnownInode(fullPath)
	if len(rest) > 0 {
		return nil
	}
	return n
}

////////////////////////////////////////////////////////////////

// Mount() generates a synthetic directory node, and mounts the file
// system there.  If opts is nil, the mount options of the root file
// system are inherited.  The encompassing filesystem should pretend
// the mount point does not exist.  If it does, it will generate an
// Inode with the same, which will cause Mount() to return EBUSY.
//
// Return values:
//
// ENOENT: the directory containing the mount point does not exist.
//
// EBUSY: the intended mount point already exists.
//
// TODO - would be useful to expose an interface to put all of the
// mount management in FileSystemConnector, so AutoUnionFs and
// MultiZipFs don't have to do it separately, with the risk of
// inconsistencies.
func (me *FileSystemConnector) Mount(mountPoint string, nodeFs NodeFileSystem, opts *FileSystemOptions) Status {
	if mountPoint == "/" || mountPoint == "" {
		me.mountRoot(nodeFs, opts)
		return OK
	}

	dirParent, base := filepath.Split(mountPoint)
	parent := me.findInode(dirParent)
	if parent == nil {
		log.Println("Could not find mountpoint parent:", dirParent)
		return ENOENT
	}

	parent.treeLock.Lock()
	defer parent.treeLock.Unlock()
	if parent.mount == nil {
		return ENOENT
	}
	node := parent.children[base]
	if node != nil {
		return EBUSY
	}

	node = me.newInode(true)
	if opts == nil {
		opts = me.rootNode.mountPoint.options
	}

	node.mountFs(nodeFs, opts)
	parent.addChild(base, node)

	if parent.mounts == nil {
		parent.mounts = make(map[string]*fileSystemMount)
	}
	parent.mounts[base] = node.mountPoint
	if me.Debug {
		log.Println("Mount: ", nodeFs, "on dir", mountPoint,
			"parent", parent)
	}
	nodeFs.Mount(me)
	me.verify()
	return OK
}

func (me *FileSystemConnector) mountRoot(nodeFs NodeFileSystem, opts *FileSystemOptions) {
	me.rootNode.mountFs(nodeFs, opts)
	nodeFs.Mount(me)
	me.verify()
}

// Unmount() tries to unmount the given path.  
//
// Returns the following error codes:
//
// EINVAL: path does not exist, or is not a mount point.
//
// EBUSY: there are open files, or submounts below this node.
func (me *FileSystemConnector) Unmount(path string) Status {
	dir, name := filepath.Split(path)
	parentNode := me.findInode(dir)
	if parentNode == nil {
		log.Println("Could not find parent of mountpoint:", path)
		return EINVAL
	}

	// Must lock parent to update tree structure.
	parentNode.treeLock.Lock()
	defer parentNode.treeLock.Unlock()

	mount := parentNode.mounts[name]
	if mount == nil {
		return EINVAL
	}

	if mount.openFiles.Count() > 0 {
		return EBUSY
	}

	mountInode := mount.mountInode
	if !mountInode.canUnmount() {
		return EBUSY
	}

	mount.mountInode = nil
	mountInode.mountPoint = nil

	parentNode.mounts[name] = nil, false
	parentNode.children[name] = nil, false
	mount.fs.Unmount()

	me.fsInit.EntryNotify(parentNode.nodeId, name)

	return OK
}

func (me *FileSystemConnector) FileNotify(path string, off int64, length int64) Status {
	node := me.findInode(path)
	if node == nil {
		return ENOENT
	}

	out := NotifyInvalInodeOut{
		Length: length,
		Off:    off,
		Ino:    node.nodeId,
	}
	return me.fsInit.InodeNotify(&out)
}

func (me *FileSystemConnector) EntryNotify(dir string, name string) Status {
	node := me.findInode(dir)
	if node == nil {
		return ENOENT
	}

	return me.fsInit.EntryNotify(node.nodeId, name)
}

func (me *FileSystemConnector) Notify(path string) Status {
	node, rest := me.findLastKnownInode(path)
	if len(rest) > 0 {
		return me.fsInit.EntryNotify(node.nodeId, rest[0])
	}
	out := NotifyInvalInodeOut{
		Ino: node.nodeId,
	}
	return me.fsInit.InodeNotify(&out)
}
