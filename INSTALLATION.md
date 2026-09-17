# Installation

Copy and paste the command for your platform into your terminal. It will automatically download and install whatsrook.

### Linux

**x86_64 (Intel / AMD):**
```bash
curl -fsSL \
  https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-linux-amd64.tar.gz \
  -o /tmp/whatsrook.tar.gz && \
  sudo tar -xzf /tmp/whatsrook.tar.gz -C /usr/local/bin && \
  rm -f /tmp/whatsrook.tar.gz && \
  sudo chmod +x /usr/local/bin/whatsrook && \
  whatsrook
```

**ARM64 (Raspberry Pi / AWS Graviton / Ampere):**
```bash
curl -fsSL \
  https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-linux-arm64.tar.gz \
  -o /tmp/whatsrook.tar.gz && \
  sudo tar -xzf /tmp/whatsrook.tar.gz -C /usr/local/bin && \
  rm -f /tmp/whatsrook.tar.gz && \
  sudo chmod +x /usr/local/bin/whatsrook && \
  whatsrook
```

### macOS

**Apple Silicon (M1 / M2 / M3 / M4):**
```bash
curl -fsSL \
  https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-darwin-arm64.tar.gz \
  -o /tmp/whatsrook.tar.gz && \
  sudo tar -xzf /tmp/whatsrook.tar.gz -C /usr/local/bin && \
  rm -f /tmp/whatsrook.tar.gz && \
  sudo chmod +x /usr/local/bin/whatsrook && \
  sudo xattr -d com.apple.quarantine /usr/local/bin/whatsrook 2>/dev/null; \
  whatsrook
```

**Intel:**
```bash
curl -fsSL \
  https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-darwin-amd64.tar.gz \
  -o /tmp/whatsrook.tar.gz && \
  sudo tar -xzf /tmp/whatsrook.tar.gz -C /usr/local/bin && \
  rm -f /tmp/whatsrook.tar.gz && \
  sudo chmod +x /usr/local/bin/whatsrook && \
  sudo xattr -d com.apple.quarantine /usr/local/bin/whatsrook 2>/dev/null; \
  whatsrook
```

### Android (Termux)

```bash
curl -fsSL \
  https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-android-arm64.tar.gz \
  -o $PREFIX/bin/wr.tar.gz && \
  tar -xzf $PREFIX/bin/wr.tar.gz -C $PREFIX/bin && \
  rm -f $PREFIX/bin/wr.tar.gz && \
  chmod +x $PREFIX/bin/whatsrook && \
  whatsrook
```

### Windows

Open **PowerShell** and paste:

**x86_64 (Intel / AMD):**
```powershell
New-Item -ItemType Directory -Force -Path "$env:LOCALAPPDATA\whatsrook" | Out-Null
curl.exe -fsSL "https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-windows-amd64.tar.gz" -o "$env:TEMP\wr.tar.gz"
tar.exe -xzf "$env:TEMP\wr.tar.gz" -C "$env:LOCALAPPDATA\whatsrook"
Remove-Item "$env:TEMP\wr.tar.gz"
& "$env:LOCALAPPDATA\whatsrook\whatsrook.exe"
```

**ARM64 (Snapdragon X Elite / Windows on ARM):**
```powershell
New-Item -ItemType Directory -Force -Path "$env:LOCALAPPDATA\whatsrook" | Out-Null
curl.exe -fsSL "https://github.com/thruqelabs/whatsapp-go/releases/latest/download/whatsrook-windows-arm64.tar.gz" -o "$env:TEMP\wr.tar.gz"
tar.exe -xzf "$env:TEMP\wr.tar.gz" -C "$env:LOCALAPPDATA\whatsrook"
Remove-Item "$env:TEMP\wr.tar.gz"
& "$env:LOCALAPPDATA\whatsrook\whatsrook.exe"
```

# Uninstalling

To uninstall and remove WhatsRook from your system:

### Linux
```bash
sudo rm -f /usr/local/bin/whatsrook ~/.local/bin/whatsrook
```

### macOS
```bash
sudo rm -f /usr/local/bin/whatsrook
```

### Android (Termux)
```bash
rm -f $PREFIX/bin/whatsrook
```

### Windows

Open **PowerShell** and paste:

```powershell
Remove-Item -Recurse -Force "$env:LOCALAPPDATA\whatsrook"
```
