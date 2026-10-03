#!/usr/bin/env python3
"""Unit tests for the lab's manifest.py and datagen.py. They need no root, ZFS or Linux:

  python3 -m unittest discover -s test/lab -v
"""
import contextlib
import errno
import hashlib
import io
import os
import shutil
import struct
import sys
import tempfile
import types
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import datagen  # noqa: E402
import manifest  # noqa: E402

OLD_ATIME_NS = 1_500_000_000_123_456_789
OLD_MTIME_NS = 1_400_000_000_987_654_321


def no_xattrs_off_linux():
    """manifest.xattrs uses os.listxattr, which Python only has on Linux."""
    if hasattr(os, "listxattr"):
        return contextlib.nullcontext()
    return mock.patch.object(manifest, "xattrs", return_value={})


class ManifestTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(lambda: shutil.rmtree(self.dir, ignore_errors=True))
        self.file = os.path.join(self.dir, "f.bin")
        with open(self.file, "wb") as f:
            f.write(b"hello" * 1000)
        os.utime(self.file, ns=(OLD_ATIME_NS, OLD_MTIME_NS))

    def test_snap_records_file_and_leaves_its_times(self):
        with no_xattrs_off_linux():
            snap = manifest.snap(self.dir)
        e = snap["entries"]["f.bin"]
        self.assertEqual(e["type"], "file")
        self.assertEqual(e["sha256"], hashlib.sha256(b"hello" * 1000).hexdigest())
        self.assertEqual(e["atime_ns"], OLD_ATIME_NS)
        self.assertEqual(e["mtime_ns"], OLD_MTIME_NS)
        st = os.lstat(self.file)
        self.assertEqual(st.st_atime_ns, OLD_ATIME_NS, "taking a snapshot changed the access time")
        self.assertEqual(st.st_mtime_ns, OLD_MTIME_NS)

    def test_snap_survives_a_file_whose_atime_cant_be_put_back(self):
        # An immutable or append-only file (or a dataset at its quota) refuses utime. The snapshot
        # must still be written, with a warning naming the file.
        real_utime, real_sha256 = os.utime, manifest.sha256

        def read_and_bump_atime(path):
            digest, _ = real_sha256(path)
            real_utime(path, ns=(OLD_ATIME_NS + 1, OLD_MTIME_NS))
            return digest, False

        for code in (errno.EPERM, errno.EDQUOT):
            with self.subTest(errno=errno.errorcode[code]):
                real_utime(self.file, ns=(OLD_ATIME_NS, OLD_MTIME_NS))
                stderr = io.StringIO()
                with no_xattrs_off_linux(), \
                        mock.patch.object(manifest, "sha256", side_effect=read_and_bump_atime), \
                        mock.patch.object(manifest.os, "utime", side_effect=OSError(code, os.strerror(code))), \
                        contextlib.redirect_stderr(stderr):
                    snap = manifest.snap(self.dir)
                self.assertEqual(snap["entries"]["f.bin"]["atime_ns"], OLD_ATIME_NS)
                self.assertIn("couldn't put back the access time", stderr.getvalue())
                self.assertIn("f.bin", stderr.getvalue())

    def test_open_quietly_uses_noatime_where_allowed(self):
        fake_noatime = 0x40000000  # a bit no real open flag here uses
        real_open = os.open
        seen = []

        def fake_open(path, flags, *args):
            seen.append(flags)
            return real_open(path, flags & ~fake_noatime, *args)

        with mock.patch.object(manifest, "O_NOATIME", fake_noatime), \
                mock.patch.object(manifest.os, "open", side_effect=fake_open):
            fd, quiet = manifest.open_quietly(self.file)
        os.close(fd)
        self.assertTrue(quiet)
        self.assertTrue(seen[0] & fake_noatime)
        self.assertTrue(seen[0] & os.O_NOFOLLOW)

    def test_open_quietly_falls_back_when_noatime_is_refused(self):
        # Linux refuses O_NOATIME with EPERM unless the caller is root or owns the file.
        fake_noatime = 0x40000000
        real_open = os.open

        def fake_open(path, flags, *args):
            if flags & fake_noatime:
                raise PermissionError(errno.EPERM, "Operation not permitted")
            return real_open(path, flags, *args)

        with mock.patch.object(manifest, "O_NOATIME", fake_noatime), \
                mock.patch.object(manifest.os, "open", side_effect=fake_open):
            fd, quiet = manifest.open_quietly(self.file)
        os.close(fd)
        self.assertFalse(quiet)

    def test_restore_atime_puts_it_back_and_keeps_mtime(self):
        os.utime(self.file, ns=(OLD_ATIME_NS + 5, OLD_MTIME_NS))
        before = types.SimpleNamespace(st_atime_ns=OLD_ATIME_NS, st_mtime_ns=0)
        self.assertTrue(manifest.restore_atime(self.file, before))
        st = os.lstat(self.file)
        self.assertEqual(st.st_atime_ns, OLD_ATIME_NS)
        self.assertEqual(st.st_mtime_ns, OLD_MTIME_NS, "restoring atime must not move mtime")

    def test_restore_atime_does_nothing_when_unchanged(self):
        before = os.lstat(self.file)
        with mock.patch.object(manifest.os, "utime", side_effect=AssertionError("utime called")):
            self.assertTrue(manifest.restore_atime(self.file, before))

    def test_restore_atime_tolerates_refusals_only(self):
        os.utime(self.file, ns=(OLD_ATIME_NS + 5, OLD_MTIME_NS))
        before = types.SimpleNamespace(st_atime_ns=OLD_ATIME_NS, st_mtime_ns=OLD_MTIME_NS)
        for code in (errno.EPERM, errno.EACCES, errno.EDQUOT, errno.ENOSPC, errno.EROFS):
            with self.subTest(errno=errno.errorcode[code]):
                stderr = io.StringIO()
                with mock.patch.object(manifest.os, "utime", side_effect=OSError(code, os.strerror(code))), \
                        contextlib.redirect_stderr(stderr):
                    self.assertFalse(manifest.restore_atime(self.file, before))
                self.assertIn("warning:", stderr.getvalue())
        with mock.patch.object(manifest.os, "utime", side_effect=OSError(errno.EIO, "I/O error")):
            with self.assertRaises(OSError):
                manifest.restore_atime(self.file, before)

    def test_diff_skip_list(self):
        before = {"entries": {"a": {"type": "file", "ino": 1, "atime_ns": 1},
                              "own-id.bin": {"type": "file", "ino": 2, "atime_ns": 1}}, "temps": []}
        after = {"entries": {"a": {"type": "file", "ino": 9, "atime_ns": 1},
                             "own-id.bin": {"type": "file", "ino": 2, "atime_ns": 1}}, "temps": []}
        problems, rewritten, kept = manifest.diff(before, after, True, set())
        self.assertEqual((rewritten, kept), (1, 1))
        self.assertEqual(problems, ["NOT rewritten (same inode): 'own-id.bin'"])
        problems, _, _ = manifest.diff(before, after, True, {"own-id.bin"})
        self.assertEqual(problems, [])


class DatagenFlagsTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.addCleanup(lambda: shutil.rmtree(self.root, ignore_errors=True))

    def make(self, setcap=None, euid=0, acl="none", tools=None, run=None, chmod=os.chmod):
        """Runs make_flag_files with chattr, setcap, ioctls and xattrs faked; returns the skip
        list, the commands run and the xattrs set. tools maps other command names to their paths."""
        skip, xattrs = [], []
        found = {"setcap": setcap, **(tools or {})}
        run = run or mock.MagicMock()
        with mock.patch.object(datagen.sys, "platform", "linux"), \
                mock.patch.object(datagen.subprocess, "run", run), \
                mock.patch.object(datagen, "ioctl_u64", return_value=None), \
                mock.patch.object(datagen.shutil, "which", side_effect=found.get), \
                mock.patch.object(datagen.os, "geteuid", return_value=euid), \
                mock.patch.object(datagen.os, "chmod", side_effect=chmod), \
                mock.patch.object(datagen.os, "setxattr", create=True,
                                  side_effect=lambda p, n, v, follow_symlinks=True: xattrs.append((p, n, v))), \
                contextlib.redirect_stdout(io.StringIO()):
            datagen.make_flag_files(self.root, skip, acl)
        return skip, [c.args[0] for c in run.call_args_list], xattrs

    def rel(self, path):
        return os.path.relpath(path, self.root)

    def test_skip_list(self):
        skip, _, _ = self.make()
        # own-id.bin (project 888 in a +P 777 folder) can't be replaced on ZFS, so it's skipped.
        for want in ("flags/projdir/own-id.bin", "flags/immutable.bin", "flags/appendonly.bin"):
            self.assertIn(want, skip)
        # Files a root run must rewrite with their metadata kept are never in the skip list.
        for rewritten in ("flags/capability.bin", "flags/trusted.bin", "flags/projdir/inherits.bin",
                          "flags/project.bin", "flags/nodump.bin"):
            self.assertNotIn(rewritten, skip)

    def test_capability_and_trusted_xattrs_without_setcap(self):
        _, cmds, xattrs = self.make(setcap=None, euid=0)
        got = {(self.rel(p), n): v for p, n, v in xattrs}
        self.assertEqual(got[("flags/capability.bin", "security.capability")],
                         datagen.capability_xattr(datagen.CAP_NET_RAW))
        self.assertIn(("flags/trusted.bin", "trusted.lab"), got)
        self.assertFalse([c for c in cmds if c[0] == "setcap"])
        self.assertTrue(os.path.isfile(os.path.join(self.root, "flags/capability.bin")))

    def test_capability_uses_setcap_when_there(self):
        _, cmds, xattrs = self.make(setcap="/usr/sbin/setcap", euid=0)
        setcaps = [c for c in cmds if c[0] == "setcap"]
        self.assertEqual(len(setcaps), 1)
        self.assertEqual(setcaps[0][1], "cap_net_raw=ep")
        self.assertEqual(self.rel(setcaps[0][2]), "flags/capability.bin")
        self.assertNotIn("security.capability", [n for _, n, _ in xattrs])

    def test_capability_file_mode_on_a_restricted_share(self):
        # On an aclmode=restricted dataset (a TrueNAS SMB share), a new file inherits a non-trivial
        # ACL and chmod is refused until that ACL is stripped. --acl nfs4 --flags must still work.
        stripped, modes = set(), {}

        def run(cmd, **_):
            if cmd[:2] == ["nfs4xdr_setfacl", "-b"]:
                stripped.add(cmd[2])
            return mock.MagicMock(returncode=0)

        def restricted_chmod(path, mode):
            if path not in stripped:
                raise PermissionError(errno.EPERM, "Operation not permitted", path)
            modes[path] = mode

        cap = os.path.join(self.root, "flags/capability.bin")
        _, cmds, _ = self.make(acl="nfs4", tools={"nfs4xdr_setfacl": "/usr/bin/nfs4xdr_setfacl"},
                               run=mock.MagicMock(side_effect=run), chmod=restricted_chmod)
        self.assertIn(["nfs4xdr_setfacl", "-b", cap], cmds)
        self.assertEqual(modes.get(cap), 0o755)

    def test_capability_file_mode_refused_without_nfs4(self):
        # Without --acl nfs4 there's no ACL to strip, so a refused chmod is a real error.
        def refuse(path, mode):
            raise PermissionError(errno.EPERM, "Operation not permitted", path)

        with self.assertRaises(PermissionError):
            self.make(acl="none", chmod=refuse)

    def test_trusted_xattr_needs_root(self):
        _, _, xattrs = self.make(euid=1000)
        self.assertNotIn("trusted.lab", [n for _, n, _ in xattrs])
        self.assertTrue(os.path.isfile(os.path.join(self.root, "flags/trusted.bin")))

    def test_capability_xattr_matches_setcap(self):
        # What `setcap cap_net_raw=ep` stores, as `getfattr -e hex -n security.capability` shows it.
        want = bytes.fromhex("0100000200200000" + "00" * 12)
        self.assertEqual(datagen.capability_xattr(datagen.CAP_NET_RAW), want)
        self.assertEqual(struct.calcsize("<5I"), 20)


if __name__ == "__main__":
    unittest.main()
