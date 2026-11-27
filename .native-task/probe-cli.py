#!/usr/bin/env python3
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile

agent, windows_amd64, windows_arm64, macos_amd64, macos_arm64 = [
    Path(arg).absolute() for arg in sys.argv[1:]
]

with tempfile.TemporaryDirectory(prefix="image-preflight-") as directory:
    root = Path(directory)
    environment = {k: v for k, v in os.environ.items() if not k.startswith("OWNSAND_AGENT_")}
    environment.update(NO_COLOR="1", PATH=str(root))
    iso = root / "placeholder.iso"
    iso.touch()
    text = root / "text"
    text.write_bytes(b"not an executable")
    unreadable = root / "unreadable"
    unreadable.write_bytes(b"not an executable")
    unreadable.chmod(0)
    missing = root / "missing-helper"
    absent = root / "absent"
    existing = root / "existing"
    existing.mkdir()
    existing.chmod(0o755)
    sentinel = existing / "sentinel"
    sentinel.write_bytes(b"keep")
    file = root / "state-file"
    file.write_bytes(b"keep")
    windows = ("--os", "windows", "--iso", iso)
    macos = ("--os", "macos")

    def invoke(state, arguments, code, diagnostic):
        command = [agent, "image", "build", "--state-dir", state, *arguments]
        result = subprocess.run(command, capture_output=True, text=True, env=environment, timeout=30)
        print(f"{state.name}: exit {result.returncode} | {result.stderr.strip()}", flush=True)
        assert result.returncode == code and diagnostic in result.stderr and not result.stdout, result

    cases = [
        (("--os", "windows", "--guest-binary", missing), 2, "needs --iso"),
        ((*windows, "--guest-binary", text), 2, "is not a Windows executable"),
        ((*macos, "--guest-binary", text), 2, "is not a macOS executable"),
        ((*windows, "--guest-binary", windows_arm64), 2, "Windows guests need GOOS=windows GOARCH=amd64"),
        ((*macos, "--guest-binary", macos_amd64), 2, "macOS guests need arm64"),
        ((*windows, "--guest-binary", missing), 1, f"open {missing}"),
        ((*macos, "--guest-binary", missing), 1, f"open {missing}"),
        ((*windows, "--guest-binary", unreadable), 1, "permission denied"),
        ((*macos, "--guest-binary", unreadable), 1, "permission denied"),
        ((*windows, "--guest-binary", root), 1, "is a directory"),
        ((*macos, "--guest-binary", root), 1, "is a directory"),
        (windows, 1, "ownsand-guest.exe was not found next to the agent or on PATH"),
        (macos, 1, "ownsand-guest was not found next to the agent or on PATH"),
    ]
    for arguments, code, diagnostic in cases:
        for state in (absent, existing, file):
            invoke(state, arguments, code, diagnostic)
            assert not absent.exists()
            assert tuple(existing.iterdir()) == (sentinel,) and sentinel.read_bytes() == b"keep"
            assert stat.S_IMODE(existing.stat().st_mode) == 0o755
            assert file.read_bytes() == b"keep"

    invoke(file, (*windows, "--guest-binary", windows_amd64), 1, f"store: create {file}")
    invoke(file, (*macos, "--guest-binary", macos_arm64), 1, f"store: create {file}")
    if sys.platform.startswith("linux"):
        control = root / "unsupported-state"
        result = subprocess.run(
            [agent, "image", "build", "--state-dir", control, *windows, "--guest-binary", windows_amd64],
            env=environment, capture_output=True, text=True, timeout=30,
        )
        print(f"unsupported host: exit {result.returncode} | {result.stderr.strip()}", flush=True)
        assert result.returncode == 2 and "not supported on this host" in result.stderr
        assert (control / "images/windows.build.lock").is_file()
        assert stat.S_IMODE(control.stat().st_mode) == 0o700
print("CLI matrix passed. Rejected inputs preserved entries, contents and POSIX mode.")
