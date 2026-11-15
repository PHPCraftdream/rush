"""git subprocess helpers for pkgmove."""

import os
import subprocess
from pathlib import Path


class Repo:
    def __init__(self, root):
        self.root = Path(root).resolve()

    def _run(self, args):
        proc = subprocess.run(
            ["git", "-C", str(self.root)] + args,
            capture_output=True,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                "git %s failed: %s" % (" ".join(args), proc.stderr.decode("utf-8", "replace"))
            )
        return proc.stdout.decode("utf-8", "replace")

    def tracked_files(self):
        out = self._run(["ls-files", "-z"])
        return sorted(p.replace("\\", "/") for p in out.split("\0") if p)

    def tracked_path_exists(self, p):
        prefix = p + "/"
        return any(f == p or f.startswith(prefix) for f in self.tracked_files())

    def dir_exists(self, p):
        return (self.root / p).is_dir()

    def git_mv(self, src, dst):
        src = src.replace("\\", "/")
        dst = dst.replace("\\", "/")
        if dst.startswith(src + "/"):
            # git cannot move a dir into itself; go via a temp sibling
            tmp = src + ".pkgmove-tmp"
            while (self.root / tmp).exists():
                tmp += "x"
            self._mv(src, tmp)
            os.makedirs((self.root / dst).parent, exist_ok=True)
            self._mv(tmp, dst)
        else:
            os.makedirs((self.root / dst).parent, exist_ok=True)
            self._mv(src, dst)

    def _mv(self, src, dst):
        proc = subprocess.run(
            ["git", "-C", str(self.root), "mv", "--", src, dst],
            capture_output=True,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                "git mv %s %s failed: %s"
                % (src, dst, proc.stderr.decode("utf-8", "replace"))
            )

    def read_go_files(self):
        return [f for f in self.tracked_files() if f.endswith(".go")]
