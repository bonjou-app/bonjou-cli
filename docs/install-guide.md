# How to Install Bonjou

Pick your operating system and follow the steps.

## One Command Installer (Auto-detect)

macOS/Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/bonjou-app/bonjou-cli/main/scripts/install.sh | bash
```

Windows (PowerShell):

```powershell
iwr https://raw.githubusercontent.com/bonjou-app/bonjou-cli/main/scripts/install.ps1 -useb | iex
```

What this does:
- Uses Homebrew on macOS if available
- Uses WinGet on Windows if available
- Falls back to Scoop on Windows if WinGet is not available or the package is not yet available there
- Falls back to direct binary install when no package manager is found

## Mac

### Option 1: Homebrew (easiest)

```bash
brew install hamzaabdulwahab/bonjou/bonjou
```

If you prefer the classic two-step flow:
```bash
brew tap hamzaabdulwahab/bonjou https://github.com/hamzaabdulwahab/homebrew-bonjou
brew install bonjou
```

Run it:
```bash
bonjou
```

Update later:
```bash
brew update && brew upgrade bonjou
```

### Option 2: Download manually

1. Download from releases:
```bash
curl -L -o bonjou https://github.com/bonjou-app/bonjou-cli/releases/download/v1.2.0/bonjou-macos
```

2. Install:
```bash
sudo mv bonjou /usr/local/bin/bonjou
sudo chmod +x /usr/local/bin/bonjou
```

3. Run:
```bash
bonjou
```

### Option 3: Build from source

```bash
git clone https://github.com/bonjou-app/bonjou-cli.git
cd bonjou-cli
go build -o bonjou ./cmd/bonjou
sudo mv bonjou /usr/local/bin/
```

### Option 4: Arch Linux (AUR)

If you use Arch Linux or an Arch-based distro:

```bash
yay -S bonjou-bin
```

Or manually from AUR:
```bash
git clone https://aur.archlinux.org/bonjou-bin.git
cd bonjou-bin
makepkg -si
```

---

## Linux

### Option 1: Debian/Ubuntu package

**Intel/AMD (most PCs):**
```bash
wget https://github.com/bonjou-app/bonjou-cli/releases/download/v1.2.0/bonjou_1.2.0_amd64.deb
sudo dpkg -i bonjou_1.2.0_amd64.deb
```

**ARM64 (Mac with Docker/Parallels, Raspberry Pi):**
```bash
wget https://github.com/bonjou-app/bonjou-cli/releases/download/v1.2.0/bonjou_1.2.0_arm64.deb
sudo dpkg -i bonjou_1.2.0_arm64.deb
```

If you get dependency errors:
```bash
sudo apt -f install
```

Run:
```bash
bonjou
```

### Option 2: Download manually

**Intel/AMD:**
```bash
curl -L -o bonjou https://github.com/bonjou-app/bonjou-cli/releases/download/v1.2.0/bonjou-linux-amd64
sudo mv bonjou /usr/local/bin/bonjou
sudo chmod +x /usr/local/bin/bonjou
```

**ARM64:**
```bash
curl -L -o bonjou https://github.com/bonjou-app/bonjou-cli/releases/download/v1.2.0/bonjou-linux-arm64
sudo mv bonjou /usr/local/bin/bonjou
sudo chmod +x /usr/local/bin/bonjou
```

### Option 3: Build from source

```bash
git clone https://github.com/bonjou-app/bonjou-cli.git
cd bonjou-cli
go build -o bonjou ./cmd/bonjou
sudo mv bonjou /usr/local/bin/
```

---

## Windows

### Option 1: WinGet (pre-installed on Windows 10/11)

WinGet ships with every modern Windows installation. Open PowerShell or Command Prompt:

First, update the WinGet package source index:
```powershell
winget source update
```

Then install using the explicit package ID:
```powershell
winget install --id HamzaAbdulWahab.Bonjou --exact
```

Run:
```powershell
bonjou
```

Update later:
```powershell
winget upgrade --id HamzaAbdulWahab.Bonjou --exact
```

Uninstall:
```powershell
winget uninstall --id HamzaAbdulWahab.Bonjou --exact
```

> **Note:** The package must be published in the [winget-pkgs community repository](https://github.com/microsoft/winget-pkgs) for this to work.
> The submission manifest lives at `packaging/winget/HamzaAbdulWahab.Bonjou.yaml` in this repo.

### Option 2: Scoop

If you have [Scoop](https://scoop.sh) installed, open PowerShell:
```powershell
scoop install https://raw.githubusercontent.com/hamzaabdulwahab/scoop-bonjou/main/bonjou.json
```

If you prefer the classic two-step flow:
```powershell
scoop bucket add bonjou https://github.com/hamzaabdulwahab/scoop-bonjou
scoop install bonjou
```

Run:
```powershell
bonjou
```

Update later:
```powershell
scoop update bonjou
```

### Option 3: Chocolatey

If you have [Chocolatey](https://community.chocolatey.org/) installed:

```powershell
choco install bonjou
```

Run:
```powershell
bonjou
```

Update later:
```powershell
choco upgrade bonjou
```

Uninstall:
```powershell
choco uninstall bonjou
```

If download fails, clear cache first:
```powershell
scoop cache rm bonjou
scoop install bonjou
```

### Option 4: Download manually

1. Download from releases:
```powershell
Invoke-WebRequest -Uri https://github.com/bonjou-app/bonjou-cli/releases/download/v1.2.0/bonjou.exe -OutFile bonjou.exe
```

2. Move it to a stable folder:
```powershell
New-Item -ItemType Directory -Force -Path C:\Tools\Bonjou | Out-Null
Move-Item .\bonjou.exe C:\Tools\Bonjou\bonjou.exe -Force
```

3. Add to PATH:
   - Open System Properties > Environment Variables
   - Add `C:\Tools\Bonjou` to your Path

4. Open new terminal and run:
```powershell
bonjou
```

### Option 5: Build from source

```powershell
git clone https://github.com/bonjou-app/bonjou-cli.git
cd bonjou-cli
go build -o bonjou.exe .\cmd\bonjou
```

Move bonjou.exe to a folder in your PATH.

---

## Check if it worked

```bash
bonjou --version
```

Should show: `1.2.0`

---

## Offline Install (USB/AirDrop)

If you dont have internet on the target machine:

1. Download the right file on another computer:
   - Mac: `bonjou-macos`
   - Linux (Intel/AMD): `bonjou-linux-amd64`
   - Linux (ARM64): `bonjou-linux-arm64`
   - Windows: `bonjou.exe`

2. Copy it to the target machine by USB, AirDrop, or any other offline method

3. Follow the matching manual install steps above

---

## Firewall

Bonjou needs these ports open:
- UDP 46320 (finding other users)
- TCP 46321 (sending messages and files)

If you cant see other users, check your firewall settings.

## Want `brew install bonjou` and `scoop install bonjou`?

See:

- [Package Registry Submission Guide](package-registry-submission.md)
