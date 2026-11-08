#!/usr/bin/env python3
"""Records a command's terminal output, with timing, as an asciicast v2 file.

    python3 scripts/record.py out.cast -- ./scripts/demo.sh ../sling

Standard library only (pty), so it runs anywhere Python 3 does. Play the result
with `asciinema play out.cast`, or turn it into an SVG with scripts/cast2svg.py.
"""
import fcntl, json, os, pty, select, struct, sys, termios, time

COLS, ROWS = 100, 40


def main() -> int:
    if len(sys.argv) < 4 or sys.argv[2] != "--":
        print(__doc__.strip(), file=sys.stderr)
        return 2
    out, cmd = sys.argv[1], sys.argv[3:]
    pid, fd = pty.fork()
    if pid == 0:
        os.environ.update(TERM="xterm-256color", COLUMNS=str(COLS), LINES=str(ROWS))
        os.execvp(cmd[0], cmd)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    start = time.monotonic()
    with open(out, "w") as f:
        f.write(json.dumps({"version": 2, "width": COLS, "height": ROWS, "timestamp": int(time.time())}) + "\n")
        while True:
            r, _, _ = select.select([fd], [], [], 0.5)
            if not r:
                if os.waitpid(pid, os.WNOHANG)[0]:
                    break
                continue
            try:
                data = os.read(fd, 4096)
            except OSError:
                break
            if not data:
                break
            text = data.decode("utf-8", "replace")
            f.write(json.dumps([round(time.monotonic() - start, 4), "o", text]) + "\n")
            sys.stdout.write(text)
            sys.stdout.flush()
    try:
        _, status = os.waitpid(pid, 0)
        return os.waitstatus_to_exitcode(status)
    except ChildProcessError:  # already reaped in the loop
        return 0


if __name__ == "__main__":
    sys.exit(main())
