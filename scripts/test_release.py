# SPDX-FileCopyrightText: 2026 Scitrera LLC
# SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
# SPDX-License-Identifier: AGPL-3.0-only
"""Exercise the release script against disposable public and nested repositories."""
import importlib.util
import os
import subprocess
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('release', Path(__file__).with_name('build-release.py'))
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.git('init', '-q')
        (self.root / 'versions.yaml').write_text('sparkroute: 0.0.1\n')
        self.git('add', '.')
        self.git('-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'fixture')

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def test_identity_rejects_internal_subtree_dirty_source_and_wrong_tag(self):
        with patch.dict(os.environ, {'GITHUB_REF_TYPE': 'tag', 'GITHUB_REF_NAME': 'v0.0.1'}):
            self.assertEqual(release.identity(self.root), ('0.0.1', self.git('rev-parse', 'HEAD')))
            nested = self.root / 'oss'
            nested.mkdir()
            with self.assertRaisesRegex(ValueError, 'independently exported'):
                release.identity(nested)
            (self.root / 'versions.yaml').write_text('sparkroute: 9.9.9\n')
            with self.assertRaisesRegex(ValueError, 'uncommitted'):
                release.identity(self.root)
            self.git('checkout', '--', 'versions.yaml')
        with patch.dict(os.environ, {'GITHUB_REF_TYPE': 'tag', 'GITHUB_REF_NAME': 'v0.0.2'}):
            with self.assertRaisesRegex(ValueError, 'tag'):
                release.identity(self.root)

    def test_archives_are_reproducible_and_retain_executable_and_notices(self):
        first, second = self.root / 'a.tgz', self.root / 'b.tgz'
        files = {'sparkroute': (b'fake executable', 0o755), 'LICENSE': (b'AGPL', 0o644)}
        release.write_archive(first, files)
        release.write_archive(second, dict(reversed(list(files.items()))))
        self.assertEqual(first.read_bytes(), second.read_bytes())
        with tarfile.open(first) as archive:
            self.assertEqual(archive.getnames(), ['LICENSE', 'sparkroute'])
            self.assertEqual(archive.getmember('sparkroute').mode, 0o755)
            self.assertEqual(archive.extractfile('LICENSE').read(), b'AGPL')

    def test_windows_zip_is_reproducible_and_contains_exe_and_notices(self):
        first, second = self.root / 'a.zip', self.root / 'b.zip'
        files = {'sparkroute.exe': (b'MZ fixture', 0o755), 'LICENSE': (b'AGPL', 0o644)}
        release.write_archive(first, files)
        release.write_archive(second, dict(reversed(list(files.items()))))
        self.assertEqual(first.read_bytes(), second.read_bytes())
        with zipfile.ZipFile(first) as archive:
            self.assertEqual(archive.read('sparkroute.exe'), b'MZ fixture')
            self.assertEqual(archive.read('LICENSE'), b'AGPL')
            self.assertEqual(archive.getinfo('sparkroute.exe').date_time, (1980, 1, 1, 0, 0, 0))

    def test_distribution_requires_full_dependency_notices(self):
        for name in release.RELEASE_NOTICES:
            (self.root / name).write_text(name)
        (self.root / 'LICENSES').mkdir()
        (self.root / 'LICENSES/BSD-3-Clause.txt').write_text('retained BSD text')
        files = release.release_notices(self.root)
        for suffix in ('.tar.gz', '.zip'):
            archive = self.root / ('notices' + suffix)
            release.write_archive(archive, files)
            if suffix == '.zip':
                with zipfile.ZipFile(archive) as handle:
                    self.assertEqual(handle.read('THIRD_PARTY_LICENSES.txt'), b'THIRD_PARTY_LICENSES.txt')
            else:
                with tarfile.open(archive) as handle:
                    self.assertEqual(handle.extractfile('THIRD_PARTY_LICENSES.txt').read(), b'THIRD_PARTY_LICENSES.txt')
        self.assertEqual(files['LICENSES/BSD-3-Clause.txt'][0], b'retained BSD text')
        (self.root / 'THIRD_PARTY_LICENSES.txt').unlink()
        with self.assertRaises(FileNotFoundError):
            release.release_notices(self.root)


if __name__ == '__main__':
    unittest.main()
