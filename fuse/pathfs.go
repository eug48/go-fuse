package fuse

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
)

var _ = log.Println

// A parent pointer: node should be reachable as parent.children[name]
type clientInodePath struct {
	parent *pathInode
	name   string
	node   *pathInode
}

// PathNodeFs is the file system that can translate an inode back to a
// path.  The path name is then used to call into an object that has
// the FileSystem interface.
//
// Lookups (ie. FileSystem.GetAttr) may return a inode number in its
// return value. The inode number ("clientInode") is used to indicate
// linked files. The clientInode is never exported back to the kernel;
// it is only used to maintain a list of all names of an inode.
type PathNodeFs struct {
	Debug     bool
	fs        FileSystem
	root      *pathInode
	connector *FileSystemConnector

	// protects clientInodeMap and pathInode.Parent pointers
	pathLock sync.RWMutex

	// This map lists all the parent links known for a given
	// nodeId.
	clientInodeMap map[uint64][]*clientInodePath

	options *PathNodeFsOptions
}

func (fs *PathNodeFs) Mount(path string, nodeFs NodeFileSystem, opts *FileSystemOptions) Status {
	dir, name := filepath.Split(path)
	if dir != "" {
		dir = filepath.Clean(dir)
	}
	parent := fs.LookupNode(dir)
	if parent == nil {
		return ENOENT
	}
	return fs.connector.Mount(parent, name, nodeFs, opts)
}

// Forgets all known information on client inodes.
func (fs *PathNodeFs) ForgetClientInodes() {
	if !fs.options.ClientInodes {
		return
	}
	fs.pathLock.Lock()
	fs.clientInodeMap = map[uint64][]*clientInodePath{}
	fs.root.forgetClientInodes()
	fs.pathLock.Unlock()
}

// Rereads all inode numbers for all known files.
func (fs *PathNodeFs) RereadClientInodes() {
	if !fs.options.ClientInodes {
		return
	}
	fs.ForgetClientInodes()
	fs.root.updateClientInodes()
}

func (fs *PathNodeFs) UnmountNode(node *Inode) Status {
	return fs.connector.Unmount(node)
}

func (fs *PathNodeFs) Unmount(path string) Status {
	node := fs.Node(path)
	if node == nil {
		return ENOENT
	}
	return fs.connector.Unmount(node)
}

func (fs *PathNodeFs) OnUnmount() {
}

func (fs *PathNodeFs) String() string {
	return fmt.Sprintf("PathNodeFs(%v)", fs.fs)
}

func (fs *PathNodeFs) OnMount(conn *FileSystemConnector) {
	fs.connector = conn
	fs.fs.OnMount(fs)
}

func (fs *PathNodeFs) Node(name string) *Inode {
	n, rest := fs.LastNode(name)
	if len(rest) > 0 {
		return nil
	}
	return n
}

// Like node, but use Lookup to discover inodes we may not have yet.
func (fs *PathNodeFs) LookupNode(name string) *Inode {
	return fs.connector.LookupNode(fs.Root().Inode(), name)
}

func (fs *PathNodeFs) Path(node *Inode) string {
	pNode := node.FsNode().(*pathInode)
	return pNode.GetPath()
}

func (fs *PathNodeFs) LastNode(name string) (*Inode, []string) {
	return fs.connector.Node(fs.Root().Inode(), name)
}

func (fs *PathNodeFs) FileNotify(path string, off int64, length int64) Status {
	node, r := fs.connector.Node(fs.root.Inode(), path)
	if len(r) > 0 {
		return ENOENT
	}
	return fs.connector.FileNotify(node, off, length)
}

func (fs *PathNodeFs) EntryNotify(dir string, name string) Status {
	node, rest := fs.connector.Node(fs.root.Inode(), dir)
	if len(rest) > 0 {
		return ENOENT
	}
	return fs.connector.EntryNotify(node, name)
}

func (fs *PathNodeFs) Notify(path string) Status {
	node, rest := fs.connector.Node(fs.root.Inode(), path)
	if len(rest) > 0 {
		return fs.connector.EntryNotify(node, rest[0])
	}
	return fs.connector.FileNotify(node, 0, 0)
}

func (fs *PathNodeFs) AllFiles(name string, mask uint32) []WithFlags {
	n := fs.Node(name)
	if n == nil {
		return nil
	}
	return n.Files(mask)
}

func NewPathNodeFs(fs FileSystem, opts *PathNodeFsOptions) *PathNodeFs {
	root := new(pathInode)
	root.fs = fs

	if opts == nil {
		opts = &PathNodeFsOptions{}
	}

	pfs := &PathNodeFs{
		fs:             fs,
		root:           root,
		clientInodeMap: map[uint64][]*clientInodePath{},
		options:        opts,
	}
	root.pathFs = pfs
	return pfs
}

func (fs *PathNodeFs) Root() FsNode {
	return fs.root
}

// This is a combination of dentry (entry in the file/directory and
// the inode). This structure is used to implement glue for FSes where
// there is a one-to-one mapping of paths and inodes.
type pathInode struct {
	pathFs *PathNodeFs
	fs     FileSystem
	Name   string

	// This is nil at the root of the mount.
	Parent *pathInode

	// This is to correctly resolve hardlinks of the underlying
	// real filesystem.
	clientInode uint64

	DefaultFsNode
}

// Drop all known client inodes. Must have the treeLock.
func (n *pathInode) forgetClientInodes() {
	n.clientInode = 0
	for _, ch := range n.Inode().FsChildren() {
		ch.FsNode().(*pathInode).forgetClientInodes()
	}
}

// Reread all client nodes below this node.  Must run outside the treeLock.
func (n *pathInode) updateClientInodes() {
	n.GetAttr(&Attr{}, nil, nil)
	for _, ch := range n.Inode().FsChildren() {
		ch.FsNode().(*pathInode).updateClientInodes()
	}
}

func (n *pathInode) LockTree() func() {
	n.pathFs.pathLock.Lock()
	return func() { n.pathFs.pathLock.Unlock() }
}

func (n *pathInode) RLockTree() func() {
	n.pathFs.pathLock.RLock()
	return func() { n.pathFs.pathLock.RUnlock() }
}

// GetPath returns the path relative to the mount governing this
// inode.  It returns nil for mount if the file was deleted or the
// filesystem unmounted.
func (n *pathInode) GetPath() (path string) {
	defer n.RLockTree()()

	rev_components := make([]string, 0, 10)
	p := n
	for ; p.Parent != nil; p = p.Parent {
		rev_components = append(rev_components, p.Name)
	}
	if p != p.pathFs.root {
		return ".deleted"
	}
	path = ReverseJoin(rev_components, "/")
	if n.pathFs.Debug {
		log.Printf("Inode %d = %q (%s)", n.Inode().nodeId, path, n.fs.String())
	}

	return path
}

func (n *pathInode) addChild(name string, child *pathInode) {
	n.Inode().AddChild(name, child.Inode())
	child.Parent = n
	child.Name = name

	if child.clientInode > 0 && n.pathFs.options.ClientInodes {
		defer n.LockTree()()
		m := n.pathFs.clientInodeMap[child.clientInode]
		e := &clientInodePath{
			n, name, child,
		}
		m = append(m, e)
		n.pathFs.clientInodeMap[child.clientInode] = m
	}
}

func (n *pathInode) rmChild(name string) *pathInode {
	childInode := n.Inode().RmChild(name)
	if childInode == nil {
		return nil
	}
	ch := childInode.FsNode().(*pathInode)

	if ch.clientInode > 0 && n.pathFs.options.ClientInodes {
		defer n.LockTree()()
		m := n.pathFs.clientInodeMap[ch.clientInode]

		idx := -1
		for i, v := range m {
			if v.parent == n && v.name == name {
				idx = i
				break
			}
		}
		if idx >= 0 {
			m[idx] = m[len(m)-1]
			m = m[:len(m)-1]
		}
		if len(m) > 0 {
			ch.Parent = m[0].parent
			ch.Name = m[0].name
			return ch
		} else {
			delete(n.pathFs.clientInodeMap, ch.clientInode)
		}
	}

	ch.Name = ".deleted"
	ch.Parent = nil

	return ch
}

// Handle a change in clientInode number for an other wise unchanged
// pathInode.
func (n *pathInode) setClientInode(ino uint64) {
	if ino == n.clientInode || !n.pathFs.options.ClientInodes {
		return
	}
	defer n.LockTree()()
	if n.clientInode != 0 {
		delete(n.pathFs.clientInodeMap, n.clientInode)
	}

	n.clientInode = ino
	if n.Parent != nil {
		e := &clientInodePath{
			n.Parent, n.Name, n,
		}
		n.pathFs.clientInodeMap[ino] = append(n.pathFs.clientInodeMap[ino], e)
	}
}

func (n *pathInode) OnForget() {
	if n.clientInode == 0 || !n.pathFs.options.ClientInodes {
		return
	}
	defer n.LockTree()()
	delete(n.pathFs.clientInodeMap, n.clientInode)
}

////////////////////////////////////////////////////////////////
// FS operations

func (n *pathInode) StatFs() *StatfsOut {
	return n.fs.StatFs(n.GetPath())
}

func (n *pathInode) Readlink(c *Context) ([]byte, Status) {
	path := n.GetPath()

	val, err := n.fs.Readlink(path, c)
	return []byte(val), err
}

func (n *pathInode) Access(mode uint32, context *Context) (code Status) {
	p := n.GetPath()
	return n.fs.Access(p, mode, context)
}

func (n *pathInode) GetXAttr(attribute string, context *Context) (data []byte, code Status) {
	return n.fs.GetXAttr(n.GetPath(), attribute, context)
}

func (n *pathInode) RemoveXAttr(attr string, context *Context) Status {
	p := n.GetPath()
	return n.fs.RemoveXAttr(p, attr, context)
}

func (n *pathInode) SetXAttr(attr string, data []byte, flags int, context *Context) Status {
	return n.fs.SetXAttr(n.GetPath(), attr, data, flags, context)
}

func (n *pathInode) ListXAttr(context *Context) (attrs []string, code Status) {
	return n.fs.ListXAttr(n.GetPath(), context)
}

func (n *pathInode) Flush(file File, openFlags uint32, context *Context) (code Status) {
	return file.Flush()
}

func (n *pathInode) OpenDir(context *Context) ([]DirEntry, Status) {
	return n.fs.OpenDir(n.GetPath(), context)
}

func (n *pathInode) Mknod(name string, mode uint32, dev uint32, context *Context) (newNode FsNode, code Status) {
	fullPath := filepath.Join(n.GetPath(), name)
	code = n.fs.Mknod(fullPath, mode, dev, context)
	if code.Ok() {
		pNode := n.createChild(false)
		newNode = pNode
		n.addChild(name, pNode)
	}
	return
}

func (n *pathInode) Mkdir(name string, mode uint32, context *Context) (newNode FsNode, code Status) {
	fullPath := filepath.Join(n.GetPath(), name)
	code = n.fs.Mkdir(fullPath, mode, context)
	if code.Ok() {
		pNode := n.createChild(true)
		newNode = pNode
		n.addChild(name, pNode)
	}
	return
}

func (n *pathInode) Unlink(name string, context *Context) (code Status) {
	code = n.fs.Unlink(filepath.Join(n.GetPath(), name), context)
	if code.Ok() {
		n.rmChild(name)
	}
	return code
}

func (n *pathInode) Rmdir(name string, context *Context) (code Status) {
	code = n.fs.Rmdir(filepath.Join(n.GetPath(), name), context)
	if code.Ok() {
		n.rmChild(name)
	}
	return code
}

func (n *pathInode) Symlink(name string, content string, context *Context) (newNode FsNode, code Status) {
	fullPath := filepath.Join(n.GetPath(), name)
	code = n.fs.Symlink(content, fullPath, context)
	if code.Ok() {
		pNode := n.createChild(false)
		newNode = pNode
		n.addChild(name, pNode)
	}
	return
}

func (n *pathInode) Rename(oldName string, newParent FsNode, newName string, context *Context) (code Status) {
	p := newParent.(*pathInode)
	oldPath := filepath.Join(n.GetPath(), oldName)
	newPath := filepath.Join(p.GetPath(), newName)
	code = n.fs.Rename(oldPath, newPath, context)
	if code.Ok() {
		ch := n.rmChild(oldName)
		p.rmChild(newName)
		p.addChild(newName, ch)
	}
	return code
}

func (n *pathInode) Link(name string, existingFsnode FsNode, context *Context) (newNode FsNode, code Status) {
	if !n.pathFs.options.ClientInodes {
		return nil, ENOSYS
	}

	newPath := filepath.Join(n.GetPath(), name)
	existing := existingFsnode.(*pathInode)
	oldPath := existing.GetPath()
	code = n.fs.Link(oldPath, newPath, context)

	var a *Attr
	if code.Ok() {
		a, code = n.fs.GetAttr(newPath, context)
	}

	if code.Ok() {
		if existing.clientInode != 0 && existing.clientInode == a.Ino {
			newNode = existing
			n.addChild(name, existing)
		} else {
			pNode := n.createChild(false)
			newNode = pNode
			pNode.clientInode = a.Ino
			n.addChild(name, pNode)
		}
	}
	return
}

func (n *pathInode) Create(name string, flags uint32, mode uint32, context *Context) (file File, newNode FsNode, code Status) {
	fullPath := filepath.Join(n.GetPath(), name)
	file, code = n.fs.Create(fullPath, flags, mode, context)
	if code.Ok() {
		pNode := n.createChild(false)
		newNode = pNode
		n.addChild(name, pNode)
	}
	return
}

func (n *pathInode) createChild(isDir bool) *pathInode {
	i := new(pathInode)
	i.fs = n.fs
	i.pathFs = n.pathFs

	n.Inode().New(isDir, i)
	return i
}

func (n *pathInode) Open(flags uint32, context *Context) (file File, code Status) {
	file, code = n.fs.Open(n.GetPath(), flags, context)
	if n.pathFs.Debug {
		file = &WithFlags{
			File:        file,
			Description: n.GetPath(),
		}
	}
	return
}

func (n *pathInode) Lookup(out *Attr, name string, context *Context) (node FsNode, code Status) {
	fullPath := filepath.Join(n.GetPath(), name)
	fi, code := n.fs.GetAttr(fullPath, context)
	if code.Ok() {
		node = n.findChild(fi, name, fullPath)
		*out = *fi
	}

	return node, code
}

func (n *pathInode) findChild(fi *Attr, name string, fullPath string) (out *pathInode) {
	if fi.Ino > 0 {
		unlock := n.RLockTree()
		v := n.pathFs.clientInodeMap[fi.Ino]
		if len(v) > 0 {
			out = v[0].node

			if fi.Nlink == 1 {
				log.Println("Found linked inode, but Nlink == 1", fullPath)
			}
		}
		unlock()
	}

	if out == nil {
		out = n.createChild(fi.IsDir())
		out.clientInode = fi.Ino
		n.addChild(name, out)
	}

	return out
}

func (n *pathInode) GetAttr(out *Attr, file File, context *Context) (code Status) {
	var fi *Attr
	if file == nil {
		// called on a deleted files.
		file = n.inode.AnyFile()
	}

	if file != nil {
		code = file.GetAttr(out)
	}

	if file == nil || code == ENOSYS || code == EBADF {
		fi, code = n.fs.GetAttr(n.GetPath(), context)
		*out = *fi
	}

	if fi != nil {
		n.setClientInode(fi.Ino)
	}

	if fi != nil && !fi.IsDir() && fi.Nlink == 0 {
		fi.Nlink = 1
	}
	return code
}

func (n *pathInode) Chmod(file File, perms uint32, context *Context) (code Status) {
	files := n.inode.Files(O_ANYWRITE)
	for _, f := range files {
		// TODO - pass context
		code = f.Chmod(perms)
		if code.Ok() {
			return
		}
	}

	if len(files) == 0 || code == ENOSYS || code == EBADF {
		code = n.fs.Chmod(n.GetPath(), perms, context)
	}
	return code
}

func (n *pathInode) Chown(file File, uid uint32, gid uint32, context *Context) (code Status) {
	files := n.inode.Files(O_ANYWRITE)
	for _, f := range files {
		// TODO - pass context
		code = f.Chown(uid, gid)
		if code.Ok() {
			return code
		}
	}
	if len(files) == 0 || code == ENOSYS || code == EBADF {
		// TODO - can we get just FATTR_GID but not FATTR_UID ?
		code = n.fs.Chown(n.GetPath(), uid, gid, context)
	}
	return code
}

func (n *pathInode) Truncate(file File, size uint64, context *Context) (code Status) {
	files := n.inode.Files(O_ANYWRITE)
	for _, f := range files {
		// TODO - pass context
		code = f.Truncate(size)
		if code.Ok() {
			return code
		}
	}
	if len(files) == 0 || code == ENOSYS || code == EBADF {
		code = n.fs.Truncate(n.GetPath(), size, context)
	}
	return code
}

func (n *pathInode) Utimens(file File, atime int64, mtime int64, context *Context) (code Status) {
	files := n.inode.Files(O_ANYWRITE)
	for _, f := range files {
		// TODO - pass context
		code = f.Utimens(atime, mtime)
		if code.Ok() {
			return code
		}
	}
	if len(files) == 0 || code == ENOSYS || code == EBADF {
		code = n.fs.Utimens(n.GetPath(), atime, mtime, context)
	}
	return code
}
