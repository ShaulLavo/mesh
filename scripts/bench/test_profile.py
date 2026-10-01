"""Exercise the scratch-only overlay against real profile output failures."""
import gzip
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class ProfileTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scratch = tempfile.TemporaryDirectory(prefix="mesh-m5-profile-", dir=os.environ.get("TMPDIR"))
        cls.root = Path(cls.scratch.name)
        cls.root.joinpath("profile.go").write_bytes(Path(__file__).with_name("profile-main.go.txt").read_bytes())
        cls.root.joinpath("main.go").write_text('''package main
import ("os"; "path/filepath"; "fmt"; "io"; "strings")
type closing struct {io.WriteCloser; fail bool}
func (w closing) Close() error { err := w.WriteCloser.Close(); if w.fail { return fmt.Errorf("fixture close failure") }; return err }
func main() {
 dir := os.Getenv("MESH_BENCH_PROFILE_DIR")
 if len(os.Args)>1 && strings.HasSuffix(os.Args[1], "-close") {
  benchCreateProfile = func(name string)(io.WriteCloser,error) { f,err := os.Create(name); return closing{f, strings.HasSuffix(name, "."+strings.TrimSuffix(os.Args[1], "-close")+".pprof")},err }
 } else if len(os.Args)>1 { os.Symlink("/dev/full", filepath.Join(dir, fmt.Sprintf("%s-%d.%s.pprof", filepath.Base(os.Args[1]), os.Getpid(), os.Args[1]))) }
 finish := benchProfile(); finish()
}
''')
        subprocess.run(["go", "build", "-o", str(cls.root / "fixture"), str(cls.root / "main.go"), str(cls.root / "profile.go")], check=True)

    @classmethod
    def tearDownClass(cls):
        cls.scratch.cleanup()

    def test_failed_profile_has_no_completion_marker(self):
        for name in ("cpu", "heap", "allocs", "cpu-close", "heap-close", "allocs-close"):
            with self.subTest(name=name), tempfile.TemporaryDirectory(dir=self.root) as directory:
                subprocess.run([str(self.root / "fixture"), name], env={**os.environ, "MESH_BENCH_PROFILE_DIR": directory}, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                self.assertFalse(list(Path(directory).glob("*.done")), name)

    def test_success_profiles_are_closed_and_valid(self):
        with tempfile.TemporaryDirectory(dir=self.root) as directory:
            subprocess.run([str(self.root / "fixture")], env={**os.environ, "MESH_BENCH_PROFILE_DIR": directory}, check=True)
            self.assertEqual(len(list(Path(directory).glob("*.done"))), 1)
            profiles = list(Path(directory).glob("*.pprof"))
            self.assertEqual(len(profiles), 3)
            for path in profiles:
                self.assertTrue(gzip.decompress(path.read_bytes()))


if __name__ == "__main__":
    unittest.main()
