//go:build windows && p22lab

// Command feas is the feasibility test of the P22-S1 addendum (ADR 0027,
// placement guards). It runs as an ordinary, non-elevated user on NTFS and
// checks four Windows behaviours the restore placement step relies on:
//
//	(a) a folder held open WITHOUT FILE_SHARE_DELETE (and with a data access
//	    right) cannot be renamed or removed by another process while held;
//	(b) a no-replace rename (MoveFileEx without MOVEFILE_REPLACE_EXISTING,
//	    FileRenameInfoEx without the replace flag) refuses an existing target;
//	(c) IsReparseTagNameSurrogate is true for a junction and a symbolic link
//	    and false for an ordinary folder;
//	(d) a delete walk that opens every entry without following it removes a
//	    junction and leaves the junction's target untouched.
//
// It creates everything under one fresh scratch folder (P22_FEAS_ROOT, default
// %TEMP%\cryo-p22-feas-<pid>), removes it with the walk of (d) at the end, and
// prints scratch paths as <T> so its output can be quoted in docs/lab.
//
// Usage (repository root; the build tag keeps it out of ./...):
//
//	go run -tags p22lab ./test/lab/p22/feas
//
// Exit 0 = every expectation held; 1 = at least one did not; 2 = setup error.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// nameSurrogateBit is the bit winnt.h's IsReparseTagNameSurrogate macro tests:
// #define IsReparseTagNameSurrogate(_tag) (((_tag) & 0x20000000))
const nameSurrogateBit = 0x20000000

// symbolicLinkFlagAllowUnprivilegedCreate is SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE
// (honoured only in Developer Mode).
const symbolicLinkFlagAllowUnprivilegedCreate = 0x2

func isNameSurrogate(tag uint32) bool { return tag&nameSurrogateBit != 0 }

// fileAttributeTagInfo is FILE_ATTRIBUTE_TAG_INFO.
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

var (
	root     string
	failures int
)

func short(p string) string { return strings.ReplaceAll(p, root, "<T>") }

func expect(name string, ok bool, detail string) {
	status := "PASS"
	if !ok {
		status = "FAIL"
		failures++
	}
	fmt.Printf("  [%s] %s: %s\n", status, name, short(detail))
}

func info(name, detail string) { fmt.Printf("  [info] %s: %s\n", name, short(detail)) }

func errno(err error) string {
	if err == nil {
		return "ok"
	}
	var e windows.Errno
	if errors.As(err, &e) {
		return fmt.Sprintf("error %d (%s)", uint32(e), e.Error())
	}
	return err.Error()
}

func u16(p string) *uint16 {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		panic(err)
	}
	return ptr
}

// openNoFollow opens the named object itself, never what a reparse point names.
func openNoFollow(path string, access, share uint32) (windows.Handle, error) {
	return windows.CreateFile(u16(path), access, share, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

func tagOfHandle(h windows.Handle) (fileAttributeTagInfo, error) {
	var ti fileAttributeTagInfo
	err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti)))
	return ti, err
}

func tagOf(path string) (fileAttributeTagInfo, error) {
	h, err := openNoFollow(path, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return fileAttributeTagInfo{}, err
	}
	defer windows.CloseHandle(h)
	return tagOfHandle(h)
}

// surrogate reports whether the object is a reparse point with a
// name-surrogate tag (junction, symbolic link, mount point).
func surrogate(ti fileAttributeTagInfo) bool {
	return ti.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && isNameSurrogate(ti.ReparseTag)
}

// deleteByHandle opens path without following it, with DELETE access, and
// marks exactly that object for deletion through its own handle.
func deleteByHandle(path string) error {
	h, err := openNoFollow(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var del uint32 = 1 // FILE_DISPOSITION_INFO.DeleteFile = TRUE
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&del)), uint32(unsafe.Sizeof(del)))
}

// removeTree is the agent-style delete walk: every entry is opened without
// following it; a reparse point is deleted itself and never descended into.
func removeTree(path string, links *int) error {
	ti, err := tagOf(path)
	if err != nil {
		return err
	}
	if ti.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		*links++
		return deleteByHandle(path)
	}
	if ti.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := removeTree(filepath.Join(path, e.Name()), links); err != nil {
				return err
			}
		}
	}
	if ti.FileAttributes&(windows.FILE_ATTRIBUTE_READONLY|windows.FILE_ATTRIBUTE_SYSTEM) != 0 {
		// Scratch clean-up only: our own test files carry these attributes.
		_ = windows.SetFileAttributes(u16(path), windows.FILE_ATTRIBUTE_NORMAL)
	}
	return deleteByHandle(path)
}

func mustMkdir(p string) {
	if err := os.MkdirAll(p, 0o755); err != nil {
		fmt.Println("setup:", err)
		os.Exit(2)
	}
}

func mustWrite(p, s string) {
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		fmt.Println("setup:", err)
		os.Exit(2)
	}
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + errno(err) + ">"
	}
	return string(b)
}

func exists(p string) bool {
	_, err := tagOf(p)
	return err == nil
}

func mklinkJ(link, target string) error {
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// child runs one operation in a separate process and reports "ok" or the error.
func child(op string, args ...string) string {
	self, err := os.Executable()
	if err != nil {
		return "child: " + err.Error()
	}
	out, _ := exec.Command(self, append([]string{"-child", op}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func runChild(op string, args []string) {
	var err error
	switch op {
	case "rename":
		err = windows.MoveFileEx(u16(args[0]), u16(args[1]), 0)
	case "rmdir":
		err = windows.RemoveDirectory(u16(args[0]))
	default:
		err = fmt.Errorf("unknown op %q", op)
	}
	fmt.Print(errno(err))
}

func sddl(p string) string {
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "<" + errno(err) + ">"
	}
	return sd.String()
}

// holdDir opens a folder without following it and without FILE_SHARE_DELETE.
func holdDir(p string, access uint32) (windows.Handle, error) {
	return openNoFollow(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
}

func testA() {
	fmt.Println("(a) a folder held open without FILE_SHARE_DELETE, renamed by another process")
	base := filepath.Join(root, "a")
	parent := filepath.Join(base, "parent")
	held := filepath.Join(parent, "held")
	mustMkdir(held)
	mustWrite(filepath.Join(base, "staged.txt"), "staged")

	h, err := holdDir(held, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		expect("open held folder", false, errno(err))
		return
	}
	r := child("rename", held, held+"-swapped")
	expect("child renames the held folder (FILE_LIST_DIRECTORY|FILE_TRAVERSE, share READ|WRITE)", r != "ok", "child: "+r)
	r = child("rmdir", held)
	expect("child removes the held folder", r != "ok", "child: "+r)
	r = child("rename", parent, parent+"-swapped")
	expect("child renames the held folder's parent", r != "ok", "child: "+r)
	if r == "ok" {
		_ = windows.MoveFileEx(u16(parent+"-swapped"), u16(parent), 0)
	}
	err = windows.MoveFileEx(u16(filepath.Join(base, "staged.txt")), u16(filepath.Join(held, "staged.txt")), 0)
	expect("holder moves a file INTO the held folder (no-replace)", err == nil, errno(err))
	windows.CloseHandle(h)
	r = child("rename", held, held+"-swapped")
	expect("control: after the handle is closed the child's rename succeeds", r == "ok", "child: "+r)
	if r == "ok" {
		_ = windows.MoveFileEx(u16(held+"-swapped"), u16(held), 0)
	}

	// Control: a handle with attribute rights only takes no part in sharing.
	h, err = holdDir(held, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		expect("open held folder (attributes only)", false, errno(err))
		return
	}
	r = child("rename", held, held+"-swapped")
	info("child renames a folder held with FILE_READ_ATTRIBUTES only (share READ|WRITE)", "child: "+r)
	windows.CloseHandle(h)
	if r == "ok" {
		_ = windows.MoveFileEx(u16(held+"-swapped"), u16(held), 0)
	}
}

// renameNoReplaceByHandle renames through FileRenameInfoEx with flags = 0.
func renameNoReplaceByHandle(from, to string, flags uint32) error {
	h, err := openNoFollow(from, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	name = name[:len(name)-1] // FileNameLength excludes the terminator
	// FILE_RENAME_INFO (amd64): Flags uint32 @0, RootDirectory HANDLE @8,
	// FileNameLength uint32 @16, FileName [1]WCHAR @20.
	const hdr = 20
	buf := make([]byte, hdr+2*len(name)+2)
	*(*uint32)(unsafe.Pointer(&buf[0])) = flags
	*(*uint32)(unsafe.Pointer(&buf[16])) = uint32(2 * len(name))
	for i, c := range name {
		*(*uint16)(unsafe.Pointer(&buf[hdr+2*i])) = c
	}
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(len(buf)))
}

func testB() {
	fmt.Println("(b) no-replace rename onto an existing name")
	base := filepath.Join(root, "b")
	mustMkdir(base)
	src, dst := filepath.Join(base, "src.txt"), filepath.Join(base, "dst.txt")
	mustWrite(src, "restored")
	mustWrite(dst, "existing")

	err := windows.MoveFileEx(u16(src), u16(dst), 0)
	expect("MoveFileEx(src, dst, 0) refused", errors.Is(err, windows.ERROR_ALREADY_EXISTS),
		errno(err)+"; dst="+strconv.Quote(read(dst))+"; src still there="+strconv.FormatBool(exists(src)))
	expect("dst content unchanged after MoveFileEx", read(dst) == "existing", strconv.Quote(read(dst)))

	err = renameNoReplaceByHandle(src, dst, 0)
	expect("FileRenameInfoEx(flags=0) refused", err != nil, errno(err)+"; dst="+strconv.Quote(read(dst)))
	expect("dst content unchanged after FileRenameInfoEx", read(dst) == "existing", strconv.Quote(read(dst)))

	err = os.Rename(src, dst)
	info("contrast: Go os.Rename(src, dst)", errno(err)+"; dst now "+strconv.Quote(read(dst)))
	mustWrite(src, "restored")
	mustWrite(dst, "existing")
	err = windows.MoveFileEx(u16(src), u16(dst), windows.MOVEFILE_REPLACE_EXISTING)
	info("control: MoveFileEx(src, dst, MOVEFILE_REPLACE_EXISTING)", errno(err)+"; dst now "+strconv.Quote(read(dst)))

	for _, a := range []struct {
		name string
		attr uint32
	}{{"read-only", windows.FILE_ATTRIBUTE_READONLY}, {"system", windows.FILE_ATTRIBUTE_SYSTEM}} {
		s, d := filepath.Join(base, "src-"+a.name+".txt"), filepath.Join(base, a.name+".txt")
		mustWrite(s, "restored")
		mustWrite(d, "existing")
		_ = windows.SetFileAttributes(u16(d), a.attr)
		err = windows.MoveFileEx(u16(s), u16(d), windows.MOVEFILE_REPLACE_EXISTING)
		info("MoveFileEx REPLACE_EXISTING onto a "+a.name+" file", errno(err)+"; target now "+strconv.Quote(read(d)))
	}
}

func testC() {
	fmt.Println("(c) IsReparseTagNameSurrogate on a junction, a symbolic link, an ordinary folder")
	base := filepath.Join(root, "c")
	target := filepath.Join(base, "target")
	mustMkdir(target)
	plain := filepath.Join(base, "plain")
	mustMkdir(plain)
	file := filepath.Join(base, "file.txt")
	mustWrite(file, "x")

	junc := filepath.Join(base, "junc")
	if err := mklinkJ(junc, target); err != nil {
		expect("create junction", false, err.Error())
	} else {
		ti, err := tagOf(junc)
		expect("junction is a name surrogate", err == nil && surrogate(ti),
			fmt.Sprintf("attrs=0x%x tag=0x%08X surrogate=%v err=%s", ti.FileAttributes, ti.ReparseTag, surrogate(ti), errno(err)))
		expect("junction tag is IO_REPARSE_TAG_MOUNT_POINT (the tag a mounted folder carries too)",
			ti.ReparseTag == windows.IO_REPARSE_TAG_MOUNT_POINT, fmt.Sprintf("tag=0x%08X", ti.ReparseTag))
	}

	sym := filepath.Join(base, "sym")
	err := windows.CreateSymbolicLink(u16(sym), u16(target),
		windows.SYMBOLIC_LINK_FLAG_DIRECTORY|symbolicLinkFlagAllowUnprivilegedCreate)
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		info("symbolic link", "NOT CREATED: "+errno(err)+" (no SeCreateSymbolicLinkPrivilege, no Developer Mode); to be proven in the lab")
	} else if err != nil {
		expect("create symbolic link", false, errno(err))
	} else {
		ti, err := tagOf(sym)
		expect("symbolic link is a name surrogate", err == nil && surrogate(ti),
			fmt.Sprintf("attrs=0x%x tag=0x%08X surrogate=%v err=%s", ti.FileAttributes, ti.ReparseTag, surrogate(ti), errno(err)))
	}

	for _, p := range []string{plain, file} {
		ti, err := tagOf(p)
		expect("ordinary "+filepath.Base(p)+" is not a name surrogate", err == nil && !surrogate(ti),
			fmt.Sprintf("attrs=0x%x reparse-point=%v surrogate=%v err=%s", ti.FileAttributes,
				ti.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, surrogate(ti), errno(err)))
	}
	info("IsReparseTagNameSurrogate(IO_REPARSE_TAG_SYMLINK 0xA000000C), constant only",
		strconv.FormatBool(isNameSurrogate(windows.IO_REPARSE_TAG_SYMLINK)))
}

func testD() {
	fmt.Println("(d) delete walk over a tree holding junctions to a folder outside it")
	base := filepath.Join(root, "d")
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(base, "tree")
	mustMkdir(outside)
	mustMkdir(filepath.Join(tree, "x", "deep"))
	victim := filepath.Join(outside, "victim.txt")
	mustWrite(victim, "outside-content")
	mustWrite(filepath.Join(tree, "x", "r.txt"), "r")
	for _, l := range []string{filepath.Join(tree, "x", "junc"), filepath.Join(tree, "x", "deep", "junc2")} {
		if err := mklinkJ(l, outside); err != nil {
			expect("create junction", false, err.Error())
			return
		}
	}
	before := sddl(victim)
	links := 0
	err := removeTree(tree, &links)
	expect("walk removed the tree", err == nil && !exists(tree), fmt.Sprintf("err=%s tree exists=%v", errno(err), exists(tree)))
	expect("walk removed both junctions as links", links == 2, fmt.Sprintf("reparse points deleted=%d", links))
	expect("target folder and its file untouched", exists(outside) && read(victim) == "outside-content",
		fmt.Sprintf("outside exists=%v victim=%q", exists(outside), read(victim)))
	expect("target file's DACL unchanged", sddl(victim) == before, "DACL equal="+strconv.FormatBool(sddl(victim) == before))
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "-child" {
		runChild(os.Args[2], os.Args[3:])
		return
	}
	root = os.Getenv("P22_FEAS_ROOT")
	if root == "" {
		root = filepath.Join(os.TempDir(), "cryo-p22-feas-"+strconv.Itoa(os.Getpid()))
	}
	if _, err := os.Lstat(root); err == nil {
		fmt.Println("setup: scratch folder already exists:", root)
		os.Exit(2)
	}
	mustMkdir(root)
	elevated := windows.GetCurrentProcessToken().IsElevated()
	fmt.Printf("feas: scratch <T>, elevated=%v\n", elevated)

	testA()
	testB()
	testC()
	testD()

	links := 0
	err := removeTree(root, &links)
	expect("clean-up: scratch removed by the same walk", err == nil && !exists(root),
		fmt.Sprintf("err=%s reparse points deleted=%d scratch exists=%v", errno(err), links, exists(root)))
	if failures > 0 {
		fmt.Printf("feas: %d expectation(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("feas: all expectations held")
}
