//go:build linux
// +build linux

package btrfs // import "github.com/docker/docker/daemon/graphdriver/btrfs"

/*
#include <stdlib.h>
#include <dirent.h>
#include <btrfs/ioctl.h>
#include <btrfs/ctree.h>

static void set_name_btrfs_ioctl_vol_args_v2(struct btrfs_ioctl_vol_args_v2* btrfs_struct, const char* value) {
    snprintf(btrfs_struct->name, BTRFS_SUBVOL_NAME_MAX, "%s", value);
}
*/
import "C"

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/containerd/containerd/pkg/userns"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/parsers"
	units "github.com/docker/go-units"
	"github.com/moby/sys/mount"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func init() {
	graphdriver.Register("btrfs", Init)
}

type btrfsOptions struct {
	minSpace uint64
	size     uint64
}

// Init returns a new BTRFS driver.
// An error is returned if BTRFS is not supported.
func Init(home string, options []string, idMap idtools.IdentityMapping) (graphdriver.Driver, error) {
	// Perform feature detection on /var/lib/docker/btrfs if it's an existing directory.
	// This covers situations where /var/lib/docker/btrfs is a mount, and on a different
	// filesystem than /var/lib/docker.
	// If the path does not exist, fall back to using /var/lib/docker for feature detection.
	testdir := home
	if _, err := os.Stat(testdir); os.IsNotExist(err) {
		testdir = filepath.Dir(testdir)
	}

	fsMagic, err := graphdriver.GetFSMagic(testdir)
	if err != nil {
		return nil, err
	}

	if fsMagic != graphdriver.FsMagicBtrfs {
		return nil, graphdriver.ErrPrerequisites
	}

	currentID := idtools.CurrentIdentity()
	dirID := idtools.Identity{
		UID: currentID.UID,
		GID: idMap.RootPair().GID,
	}

	if err := idtools.MkdirAllAndChown(home, 0710, dirID); err != nil {
		return nil, err
	}

	opt, userDiskQuota, err := parseOptions(options)
	if err != nil {
		return nil, err
	}

	// For some reason shared mount propagation between a container
	// and the host does not work for btrfs, and a remedy is to bind
	// mount graphdriver home to itself (even without changing the
	// propagation mode).
	err = mount.MakeMount(home)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to make %s a mount", home)
	}

	driver := &Driver{
		home:    home,
		idMap:   idMap,
		options: opt,
	}

	if userDiskQuota {
		if err := driver.enableQuota(); err != nil {
			return nil, err
		}
	}

	return graphdriver.NewNaiveDiffDriver(driver, driver.idMap), nil
}

func parseOptions(opt []string) (btrfsOptions, bool, error) {
	var options btrfsOptions
	userDiskQuota := false
	for _, option := range opt {
		key, val, err := parsers.ParseKeyValueOpt(option)
		if err != nil {
			return options, userDiskQuota, err
		}
		key = strings.ToLower(key)
		switch key {
		case "btrfs.min_space":
			minSpace, err := units.RAMInBytes(val)
			if err != nil {
				return options, userDiskQuota, err
			}
			userDiskQuota = true
			options.minSpace = uint64(minSpace)
		default:
			return options, userDiskQuota, fmt.Errorf("Unknown option %s", key)
		}
	}
	return options, userDiskQuota, nil
}

// Driver contains information about the filesystem mounted.
type Driver struct {
	// root of the file system
	home         string
	idMap        idtools.IdentityMapping
	options      btrfsOptions
	quotaEnabled bool
	once         sync.Once
}

// String prints the name of the driver (btrfs).
func (d *Driver) String() string {
	return "btrfs"
}

// Status returns current driver information in a two dimensional string array.
// Output contains "Build Version" and "Library Version" of the btrfs libraries used.
// Version information can be used to check compatibility with your kernel.
func (d *Driver) Status() [][2]string {
	status := [][2]string{}
	if bv := btrfsBuildVersion(); bv != "-" {
		status = append(status, [2]string{"Build Version", bv})
	}
	if lv := btrfsLibVersion(); lv != -1 {
		status = append(status, [2]string{"Library Version", fmt.Sprintf("%d", lv)})
	}
	return status
}

// GetMetadata returns empty metadata for this driver.
func (d *Driver) GetMetadata(id string) (map[string]string, error) {
	return nil, nil
}

// Cleanup unmounts the home directory.
func (d *Driver) Cleanup() error {
	if err := mount.Unmount(d.home); err != nil {
		return err
	}

	return nil
}

func free(p *C.char) {
	C.free(unsafe.Pointer(p))
}

func openDir(path string) (*C.DIR, error) {
	Cpath := C.CString(path)
	defer free(Cpath)

	dir := C.opendir(Cpath)
	if dir == nil {
		return nil, fmt.Errorf("Can't open dir")
	}
	return dir, nil
}

func closeDir(dir *C.DIR) {
	if dir != nil {
		C.closedir(dir)
	}
}

func getDirFd(dir *C.DIR) uintptr {
	return uintptr(C.dirfd(dir))
}

func subvolCreate(path, name string) error {
	dir, err := openDir(path)
	if err != nil {
		return err
	}
	defer closeDir(dir)

	var args C.struct_btrfs_ioctl_vol_args
	for i, c := range []byte(name) {
		args.name[i] = C.char(c)
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_SUBVOL_CREATE,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to create btrfs subvolume: %v", errno.Error())
	}
	return nil
}

func subvolSnapshot(src, dest, name string) error {
	srcDir, err := openDir(src)
	if err != nil {
		return err
	}
	defer closeDir(srcDir)

	destDir, err := openDir(dest)
	if err != nil {
		return err
	}
	defer closeDir(destDir)

	var args C.struct_btrfs_ioctl_vol_args_v2
	args.fd = C.__s64(getDirFd(srcDir))

	var cs = C.CString(name)
	C.set_name_btrfs_ioctl_vol_args_v2(&args, cs)
	C.free(unsafe.Pointer(cs))

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(destDir), C.BTRFS_IOC_SNAP_CREATE_V2,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to create btrfs snapshot: %v", errno.Error())
	}
	return nil
}

func isSubvolume(p string) (bool, error) {
	var bufStat unix.Stat_t
	if err := unix.Lstat(p, &bufStat); err != nil {
		return false, err
	}

	// return true if it is a btrfs subvolume
	return bufStat.Ino == C.BTRFS_FIRST_FREE_OBJECTID, nil
}

func subvolDelete(dirpath, name string, quotaEnabled bool) error {
	dir, err := openDir(dirpath)
	if err != nil {
		return err
	}
	defer closeDir(dir)
	fullPath := path.Join(dirpath, name)

	var args C.struct_btrfs_ioctl_vol_args

	// walk the btrfs subvolumes
	walkSubVolumes := func(p string, f os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p != fullPath {
				// missing most likely because the path was a subvolume that got removed in the previous iteration
				// since it's gone anyway, we don't care
				return nil
			}
			return fmt.Errorf("error walking subvolumes: %v", err)
		}
		// we want to check children only so skip itself
		// it will be removed after the filepath walk anyways
		if f.IsDir() && p != fullPath {
			sv, err := isSubvolume(p)
			if err != nil {
				return fmt.Errorf("Failed to test if %s is a btrfs subvolume: %v", p, err)
			}
			if sv {
				if err := subvolDelete(path.Dir(p), f.Name(), quotaEnabled); err != nil {
					return fmt.Errorf("Failed to destroy btrfs child subvolume (%s) of parent (%s): %v", p, dirpath, err)
				}
			}
		}
		return nil
	}
	if err := filepath.WalkDir(path.Join(dirpath, name), walkSubVolumes); err != nil {
		return fmt.Errorf("Recursively walking subvolumes for %s failed: %v", dirpath, err)
	}

	if quotaEnabled {
		if qgroupid, err := subvolLookupQgroup(fullPath); err == nil {
			var args C.struct_btrfs_ioctl_qgroup_create_args
			args.qgroupid = C.__u64(qgroupid)

			_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_QGROUP_CREATE,
				uintptr(unsafe.Pointer(&args)))
			if errno != 0 {
				logrus.WithField("storage-driver", "btrfs").Errorf("Failed to delete btrfs qgroup %v for %s: %v", qgroupid, fullPath, errno.Error())
			}
		} else {
			logrus.WithField("storage-driver", "btrfs").Errorf("Failed to lookup btrfs qgroup for %s: %v", fullPath, err.Error())
		}
	}

	// all subvolumes have been removed
	// now remove the one originally passed in
	for i, c := range []byte(name) {
		args.name[i] = C.char(c)
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_SNAP_DESTROY,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to destroy btrfs snapshot %s for %s: %v", dirpath, name, errno.Error())
	}
	return nil
}

func (d *Driver) updateQuotaStatus() {
	d.once.Do(func() {
		if !d.quotaEnabled {
			// In case quotaEnabled is not set, check qgroup and update quotaEnabled as needed
			if err := qgroupStatus(d.home); err != nil {
				// quota is still not enabled
				return
			}
			d.quotaEnabled = true
		}
	})
}

func (d *Driver) enableQuota() error {
	d.updateQuotaStatus()

	if d.quotaEnabled {
		return nil
	}

	dir, err := openDir(d.home)
	if err != nil {
		return err
	}
	defer closeDir(dir)

	var args C.struct_btrfs_ioctl_quota_ctl_args
	args.cmd = C.BTRFS_QUOTA_CTL_ENABLE
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_QUOTA_CTL,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to enable btrfs quota for %s: %v", dir, errno.Error())
	}

	d.quotaEnabled = true

	return nil
}

func (d *Driver) subvolRescanQuota() error {
	d.updateQuotaStatus()

	if !d.quotaEnabled {
		return nil
	}

	dir, err := openDir(d.home)
	if err != nil {
		return err
	}
	defer closeDir(dir)

	var args C.struct_btrfs_ioctl_quota_rescan_args
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_QUOTA_RESCAN_WAIT,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to rescan btrfs quota for %s: %v", dir, errno.Error())
	}

	return nil
}

func subvolLimitQgroup(path string, size uint64) error {
	dir, err := openDir(path)
	if err != nil {
		return err
	}
	defer closeDir(dir)

	var args C.struct_btrfs_ioctl_qgroup_limit_args
	args.lim.max_referenced = C.__u64(size)
	args.lim.flags = C.BTRFS_QGROUP_LIMIT_MAX_RFER
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_QGROUP_LIMIT,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to limit qgroup for %s: %v", dir, errno.Error())
	}

	return nil
}

// qgroupStatus performs a BTRFS_IOC_TREE_SEARCH on the root path
// with search key of BTRFS_QGROUP_STATUS_KEY.
// In case qgroup is enabled, the retuned key type will match BTRFS_QGROUP_STATUS_KEY.
// For more details please see https://github.com/kdave/btrfs-progs/blob/v4.9/qgroup.c#L1035
func qgroupStatus(path string) error {
	dir, err := openDir(path)
	if err != nil {
		return err
	}
	defer closeDir(dir)

	var args C.struct_btrfs_ioctl_search_args
	args.key.tree_id = C.BTRFS_QUOTA_TREE_OBJECTID
	args.key.min_type = C.BTRFS_QGROUP_STATUS_KEY
	args.key.max_type = C.BTRFS_QGROUP_STATUS_KEY
	args.key.max_objectid = C.__u64(math.MaxUint64)
	args.key.max_offset = C.__u64(math.MaxUint64)
	args.key.max_transid = C.__u64(math.MaxUint64)
	args.key.nr_items = 4096

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_TREE_SEARCH,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return fmt.Errorf("Failed to search qgroup for %s: %v", path, errno.Error())
	}
	sh := (*C.struct_btrfs_ioctl_search_header)(unsafe.Pointer(&args.buf))
	if sh._type != C.BTRFS_QGROUP_STATUS_KEY {
		return fmt.Errorf("Invalid qgroup search header type for %s: %v", path, sh._type)
	}
	return nil
}

func subvolLookupQgroup(path string) (uint64, error) {
	dir, err := openDir(path)
	if err != nil {
		return 0, err
	}
	defer closeDir(dir)

	var args C.struct_btrfs_ioctl_ino_lookup_args
	args.objectid = C.BTRFS_FIRST_FREE_OBJECTID

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.BTRFS_IOC_INO_LOOKUP,
		uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return 0, fmt.Errorf("Failed to lookup qgroup for %s: %v", dir, errno.Error())
	}
	if args.treeid == 0 {
		return 0, fmt.Errorf("Invalid qgroup id for %s: 0", dir)
	}

	return uint64(args.treeid), nil
}

func (d *Driver) subvolumesDir() string {
	return path.Join(d.home, "subvolumes")
}

func (d *Driver) subvolumesDirID(id string) string {
	return path.Join(d.subvolumesDir(), id)
}

func (d *Driver) quotasDir() string {
	return path.Join(d.home, "quotas")
}

func (d *Driver) quotasDirID(id string) string {
	return path.Join(d.quotasDir(), id)
}

// CreateReadWrite creates a layer that is writable for use as a container
// file system.
func (d *Driver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	return d.Create(id, parent, opts)
}

// Create the filesystem with given id.
func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	quotas := path.Join(d.home, "quotas")
	subvolumes := path.Join(d.home, "subvolumes")
	root := d.idMap.RootPair()

	currentID := idtools.CurrentIdentity()
	dirID := idtools.Identity{
		UID: currentID.UID,
		GID: root.GID,
	}

	if err := idtools.MkdirAllAndChown(subvolumes, 0710, dirID); err != nil {
		return err
	}
	if parent == "" {
		if err := subvolCreate(subvolumes, id); err != nil {
			return err
		}
	} else {
		parentDir := d.subvolumesDirID(parent)
		st, err := os.Stat(parentDir)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s: not a directory", parentDir)
		}
		if err := subvolSnapshot(parentDir, subvolumes, id); err != nil {
			return err
		}
	}

	var storageOpt map[string]string
	if opts != nil {
		storageOpt = opts.StorageOpt
	}

	if _, ok := storageOpt["size"]; ok {
		driver := &Driver{}
		if err := d.parseStorageOpt(storageOpt, driver); err != nil {
			return err
		}

		if err := d.setStorageSize(path.Join(subvolumes, id), driver); err != nil {
			return err
		}
		if err := idtools.MkdirAllAndChown(quotas, 0700, idtools.CurrentIdentity()); err != nil {
			return err
		}
		if err := os.WriteFile(path.Join(quotas, id), []byte(fmt.Sprint(driver.options.size)), 0644); err != nil {
			return err
		}
	}

	// if we have a remapped root (user namespaces enabled), change the created snapshot
	// dir ownership to match
	if root.UID != 0 || root.GID != 0 {
		if err := root.Chown(path.Join(subvolumes, id)); err != nil {
			return err
		}
	}

	mountLabel := ""
	if opts != nil {
		mountLabel = opts.MountLabel
	}

	return label.Relabel(path.Join(subvolumes, id), mountLabel, false)
}

// Parse btrfs storage options
func (d *Driver) parseStorageOpt(storageOpt map[string]string, driver *Driver) error {
	// Read size to change the subvolume disk quota per container
	for key, val := range storageOpt {
		key := strings.ToLower(key)
		switch key {
		case "size":
			size, err := units.RAMInBytes(val)
			if err != nil {
				return err
			}
			driver.options.size = uint64(size)
		default:
			return fmt.Errorf("Unknown option %s", key)
		}
	}

	return nil
}

// Set btrfs storage size
func (d *Driver) setStorageSize(dir string, driver *Driver) error {
	if driver.options.size == 0 {
		return fmt.Errorf("btrfs: invalid storage size: %s", units.HumanSize(float64(driver.options.size)))
	}
	if d.options.minSpace > 0 && driver.options.size < d.options.minSpace {
		return fmt.Errorf("btrfs: storage size cannot be less than %s", units.HumanSize(float64(d.options.minSpace)))
	}
	if err := d.enableQuota(); err != nil {
		return err
	}
	return subvolLimitQgroup(dir, driver.options.size)
}

// Remove the filesystem with given id.
func (d *Driver) Remove(id string) error {
	dir := d.subvolumesDirID(id)
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	quotasDir := d.quotasDirID(id)
	if _, err := os.Stat(quotasDir); err == nil {
		if err := os.Remove(quotasDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// Call updateQuotaStatus() to invoke status update
	d.updateQuotaStatus()

	if err := subvolDelete(d.subvolumesDir(), id, d.quotaEnabled); err != nil {
		if d.quotaEnabled {
			// use strings.Contains() rather than errors.Is(), because subvolDelete() does not use %w yet
			if userns.RunningInUserNS() && strings.Contains(err.Error(), "operation not permitted") {
				err = errors.Wrap(err, `failed to delete subvolume without root (hint: remount btrfs on "user_subvol_rm_allowed" option, or update the kernel to >= 4.18, or change the storage driver to "fuse-overlayfs")`)
			}
			return err
		}
		// If quota is not enabled, fallback to rmdir syscall to delete subvolumes.
		// This would allow unprivileged user to delete their owned subvolumes
		// in kernel >= 4.18 without user_subvol_rm_allowed mount option.
		//
		// From https://github.com/containers/storage/pull/508/commits/831e32b6bdcb530acc4c1cb9059d3c6dba14208c
	}
	if err := containerfs.EnsureRemoveAll(dir); err != nil {
		return err
	}
	return d.subvolRescanQuota()
}

// Get the requested filesystem id.
func (d *Driver) Get(id, mountLabel string) (string, error) {
	dir := d.subvolumesDirID(id)
	st, err := os.Stat(dir)
	if err != nil {
		return "", err
	}

	if !st.IsDir() {
		return "", fmt.Errorf("%s: not a directory", dir)
	}

	if quota, err := os.ReadFile(d.quotasDirID(id)); err == nil {
		if size, err := strconv.ParseUint(string(quota), 10, 64); err == nil && size >= d.options.minSpace {
			if err := d.enableQuota(); err != nil {
				return "", err
			}
			if err := subvolLimitQgroup(dir, size); err != nil {
				return "", err
			}
		}
	}

	return dir, nil
}

// Put is not implemented for BTRFS as there is no cleanup required for the id.
func (d *Driver) Put(id string) error {
	// Get() creates no runtime resources (like e.g. mounts)
	// so this doesn't need to do anything.
	return nil
}

// Exists checks if the id exists in the filesystem.
func (d *Driver) Exists(id string) bool {
	dir := d.subvolumesDirID(id)
	_, err := os.Stat(dir)
	return err == nil
}
