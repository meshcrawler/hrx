"""Render real isolated terminal sessions; never capture the user's desktop.

Run: uv run --with pillow --with pyte python scripts/capture_screenshots.py
Requires the installed hrx, populated cache, zsh, fzf-tab and macOS Menlo.
"""

from __future__ import annotations

import fcntl
import os
from pathlib import Path
import pty
import select
import shlex
import signal
import struct
import subprocess
import tempfile
import termios
import time

from PIL import Image, ImageDraw, ImageFont
import pyte


ROOT = Path(__file__).resolve().parent.parent
OUTPUT = ROOT / "docs" / "screenshots"
FONT = "/System/Library/Fonts/Menlo.ttc"
COLORS = {
    "black": "101923", "red": "ff8080", "green": "79d9b3",
    "brown": "e8c77b", "blue": "89b4fa", "magenta": "d7aaf5",
    "cyan": "7ed7df", "white": "e1e9f2", "brightblack": "8192a4",
    "brightred": "ffb4ab", "brightgreen": "a8f0cb", "brightbrown": "ffe1a3",
    "brightblue": "bdd5ff", "brightmagenta": "efd0ff", "brightcyan": "b2f1f5",
    "brightwhite": "ffffff",
}


def color(value: str, default: str) -> str:
    if value == "default":
        return default
    return "#" + COLORS.get(value, value)


class Terminal:
    def __init__(self, directory: str):
        self.screen = pyte.Screen(100, 30)
        self.stream = pyte.ByteStream(self.screen)
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            environment = dict(os.environ, ZDOTDIR=directory, TERM="xterm-256color")
            os.execve("/bin/zsh", ["zsh", "-df"], environment)
        # fzf asks its terminal for the cursor position before drawing inline.
        self.screen.write_process_input = lambda value: os.write(self.fd, value.encode())
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))
        self.drain(0.2)
        completion_dir = Path.home() / ".local/share/zsh/site-functions"
        plugin = Path.home() / ".oh-my-zsh/custom/plugins/fzf-tab/fzf-tab.plugin.zsh"
        setup = (
            "PS1='hrx> '; unsetopt PROMPT_SP; "
            f"fpath=({shlex.quote(str(completion_dir))} $fpath); "
            "autoload -Uz compinit; compinit -D -i; "
            f"source {shlex.quote(str(plugin))}; "
            "zstyle ':completion:*' menu no; print CAPTURE_READY\r"
        )
        self.send(setup)
        self.wait_for("CAPTURE_READY", timeout=10)
        self.drain(0.5)
        self.clear()

    def send(self, value: str):
        os.write(self.fd, value.encode())

    def drain(self, seconds: float):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            ready, _, _ = select.select([self.fd], [], [], min(0.05, max(0, until - time.monotonic())))
            if ready:
                self.stream.feed(os.read(self.fd, 65536))

    def wait_for(self, value: str, timeout: float = 30):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            self.drain(0.1)
            if value in "\n".join(self.screen.display):
                self.drain(0.4)
                return
        raise RuntimeError(f"terminal did not show expected marker: {value}")

    def clear(self):
        self.send("clear\r")
        self.drain(0.3)

    def capture(self, name: str, title: str):
        self.drain(0.2)
        font = ImageFont.truetype(FONT, 20)
        bold = ImageFont.truetype(FONT, 20, index=1)
        cell = font.getlength("M")
        used = [index for index, line in enumerate(self.screen.display) if line.strip()]
        rows = max(used, default=0) + 2
        width = round(cell * self.screen.columns) + 56
        height = 78 + rows * 28
        image = Image.new("RGB", (width, height), "#101923")
        draw = ImageDraw.Draw(image)
        draw.rectangle((0, 0, width, 48), fill="#1d2c3b")
        draw.text((28, 12), title, font=bold, fill="#b9f0dd")
        for y in range(rows - 1):
            for x in range(self.screen.columns):
                char = self.screen.buffer[y][x]
                fg = color(char.fg, "#e1e9f2")
                bg = color(char.bg, "#101923")
                if char.reverse:
                    fg, bg = bg, fg
                left, top = 28 + round(x * cell), 64 + y * 28
                if bg != "#101923":
                    draw.rectangle((left, top, left + round(cell), top + 28), fill=bg)
                draw.text((left, top), char.data, font=bold if char.bold else font, fill=fg)
        OUTPUT.mkdir(parents=True, exist_ok=True)
        image.save(OUTPUT / name)
        print(f"Captured {name} ({width}x{height})")

    def close(self):
        os.kill(self.pid, signal.SIGTERM)
        os.close(self.fd)
        os.waitpid(self.pid, 0)


def main():
    model_count = len(subprocess.check_output(["hrx", "_complete", "models"], text=True).splitlines())
    effort_count = len(subprocess.check_output(["hrx", "_complete", "reasoning", "gpt-6.1-sol"], text=True).splitlines())
    if not model_count or not effort_count:
        raise RuntimeError("run hrx refresh before capturing the example pickers")
    with tempfile.TemporaryDirectory(prefix="hrx-capture-") as directory:
        terminal = Terminal(directory)
        try:
            terminal.send("hrx models\r")
            terminal.wait_for("tfy-contoso/gpt-6.1-sol")
            terminal.capture("models.png", "hrx models | Shared catalog, authorized routes")
            terminal.clear()
            terminal.send("hrx codex --model gpt-6.1-sol --reasoning high --dry-run\r")
            terminal.wait_for('"passthrough": null')
            terminal.capture("dry-run.png", "hrx dry-run | See exactly where the model will run")
            terminal.clear()
            terminal.send("hrx codex --model gpt-\t")
            terminal.wait_for(f"{model_count}/{model_count}")
            terminal.capture("model-picker.png", "fzf-tab | Pick an authorized model")
            terminal.send("\x03")
            terminal.drain(0.2)
            terminal.send("\x03")
            terminal.drain(0.2)
            terminal.clear()
            terminal.send("hrx codex --model gpt-6.1-sol --reasoning \t")
            terminal.wait_for(f"{effort_count}/{effort_count}")
            terminal.capture("reasoning-picker.png", "fzf-tab | Reasoning levels for the selected model")
            terminal.send("\x03")
            terminal.drain(0.2)
            terminal.send("\x03")
            terminal.drain(0.2)
            terminal.clear()
            terminal.send("hrx models --harness claude\r")
            terminal.wait_for("claude-sonnet-5")
            terminal.send("hrx catalog --harness opencode --search glm-5p3\r")
            terminal.wait_for("fw-acme-glm-5p3")
            terminal.capture("harness-models.png", "hrx | One catalog, harness-specific model lists")
            terminal.clear()
            terminal.send("hrx opencode --model glm-5p3 --dry-run\r")
            terminal.wait_for('"passthrough": null')
            terminal.capture("opencode-dry-run.png", "OpenCode + GLM | Validated routing, no credentials displayed")
            terminal.clear()
            count = len(subprocess.check_output(["hrx", "_complete", "models", "opencode"], text=True).splitlines())
            terminal.send("hrx opencode --model \t")
            terminal.wait_for(f"{count}/{count}")
            terminal.capture("opencode-picker.png", "fzf-tab | OpenCode models from the shared catalog")
            terminal.send("\x03")
            terminal.drain(0.2)
            terminal.send("\x03")
            terminal.drain(0.2)
            terminal.clear()
            count = len(subprocess.check_output(["hrx", "_complete", "models", "claude"], text=True).splitlines())
            terminal.send("hrx claude --model claude-\t")
            terminal.wait_for(f"{count}/{count}")
            terminal.capture("claude-picker.png", "fzf-tab | All five permitted Claude models")
        finally:
            terminal.close()


if __name__ == "__main__":
    main()
