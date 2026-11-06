#!/usr/bin/env python3
"""Create an ignored, mode-0600 config file without displaying any secrets."""
import base64
import os
from pathlib import Path
import secrets

root = Path(__file__).resolve().parent.parent
directory = root / ".secrets"
directory.mkdir(mode=0o700, exist_ok=True)
os.chmod(directory, 0o700)
target = directory / "config.env"
template = (root / ".env.example").read_text()
values = {
    "API_TOKEN": secrets.token_urlsafe(32),
    "TOKEN_ENCRYPTION_KEY": base64.b64encode(secrets.token_bytes(32)).decode(),
    "OURA_WEBHOOK_VERIFICATION_TOKEN": secrets.token_urlsafe(32),
}
text = "\n".join(
    f"{line.split('=', 1)[0]}={values[line.split('=', 1)[0]]}"
    if "=" in line and line.split("=", 1)[0] in values else line
    for line in template.splitlines()
) + "\n"
fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as file:
    file.write(text)
print(f"Created {target}; fill in the database URLs and Oura credentials locally.")
