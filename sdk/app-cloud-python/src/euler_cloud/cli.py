"""CLI credentials come from environment or a private file, never an argument."""

import argparse
import json
import os
from pathlib import Path
import stat
import sys

from .client import APIError, Client


def _token():
    filename = os.environ.get("EULER_SERVICE_TOKEN_FILE")
    if not filename:
        return os.environ.get("EULER_SERVICE_TOKEN", "")
    path = Path(filename)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or (os.name == "posix" and info.st_mode & 0o077):
        raise ValueError("the token file must be a regular private file (mode 0600)")
    if info.st_size > 4096:
        raise ValueError("the token file is too large")
    return path.read_text().strip()


def main(argv=None):
    parser = argparse.ArgumentParser(description="Euler project service-account CLI")
    parser.add_argument("--url", default=os.environ.get("EULER_URL"))
    parser.add_argument("--tenant", default=os.environ.get("EULER_TENANT_ID"))
    parser.add_argument("--project", default=os.environ.get("EULER_PROJECT_ID"))
    parser.add_argument("--allow-insecure-loopback", action="store_true")
    commands = parser.add_subparsers(dest="command", required=True)
    request = commands.add_parser("request", help="Call an application machine API route")
    request.add_argument("application")
    request.add_argument("method", choices=["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"])
    request.add_argument("path")
    request.add_argument("--query", action="append", default=[], metavar="KEY=VALUE")
    request.add_argument("--data-file", type=Path, help="UTF-8 JSON request body")
    request.add_argument("--output", type=Path, help="Save response bytes or JSON to a new private file")
    for name in ("databases", "buckets", "statistics-sites"):
        commands.add_parser(name)
    quota = commands.add_parser("quota")
    quota.add_argument("application", choices=["database", "storage"])
    args = parser.parse_args(argv)
    if not all((args.url, args.tenant, args.project)):
        parser.error("url, tenant and project must be set by options or EULER_* environment variables")
    try:
        client = Client(args.url, args.tenant, args.project, _token(), allow_insecure_loopback=args.allow_insecure_loopback)
        if args.command == "request":
            query = {}
            for pair in args.query:
                key, separator, value = pair.partition("=")
                if not separator or not key:
                    raise ValueError("query parameters must have KEY=VALUE form")
                query.setdefault(key, []).append(value)
            data = json.loads(args.data_file.read_text()) if args.data_file else None
            value = client.request(args.application, args.method, args.path, data=data, query=query).data
        elif args.command == "quota":
            value = client.quota(args.application)
        else:
            value = getattr(client, args.command.replace("-", "_"))()
        payload = value if isinstance(value, bytes) else (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode()
        output = getattr(args, "output", None)
        if output:
            # Exclusive create avoids clobbering a token, source file, or prior export.
            fd = os.open(output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(fd, "wb") as file:
                file.write(payload)
        else:
            sys.stdout.buffer.write(payload)
        return 0
    except APIError as error:
        print(json.dumps({"error": error.code, "status": error.status, "message": str(error), "requestId": error.request_id}, ensure_ascii=False), file=sys.stderr)
        return 1
    except (ValueError, OSError):
        print("Configuration or input is invalid; check the origin, project, token file and JSON body.", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
