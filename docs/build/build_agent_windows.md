# Building the yc-360 Script on Windows

This guide provides step-by-step instructions to build the **yc-360 Script** on a Windows machine. The yc-360 script includes Go and C code, so it requires a proper toolchain setup.

### Step 1: Download MSYS2

**MSYS2** is a software distribution and development platform for Windows that provides a Unix-like shell and access to commonly used Linux development tools through its `pacman` package manager.

👉 Download the MSYS2 installer from [here](https://github.com/msys2/msys2-installer/releases/download/2021-07-25/msys2-x86_64-20210725.exe).

### Step 2: Install MSYS2
- Double-click the downloaded `.exe` file to launch the installation wizard.
- Choose the installation path (e.g., `C:\msys64`) and click **Next**.
- Continue with the default settings and click **Next** through each step until you reach the final screen.
- Before clicking **Finish**, check the box **"Run MSYS2 now"**

![img](/docs/images/installation-folder.png)

Proceed to the next step once the **MSYS2** windows is opened.
### Step 3: Install/Update Package Database
>💡**What is package database?**
>
>The package database is a local index maintained by `pacman`. It tracks:
>-  Available packages and versions
>- Dependencies each package needs
>- Metadata like descriptions, file lists, and installation scripts

To install/update it, run the following command in the MSYS2 terminal:
```
packman -Syu
```
![img](/docs/images/update-db-package.png)

When you get an output as shown in the above screenshot, close the window and reopen it by searching **MSYS2 MSYS** using Windows search.

### Step 4: Install/Update Base Package
Once the terminal is reopened, upgrade base packages by running:
```
packman -Su
```
 ![img](/docs/images/update-package.png)

### Step 5: Install MinGW-w64

> ### 💡 **What is MinGW-w64?**
> 
> **MinGW-w64** (Minimalist GNU for Windows – 64-bit) is a toolchain that includes:
> -   **GCC (GNU Compiler Collection)** for compiling C, C++, and other languages
> - Linkers, debuggers, and other build utilities
> - Native Windows headers and runtime libraries

It’s required because the yc-360 script includes C modules, and Go’s `cgo` relies on an available C compiler to build them. Windows doesn’t include a native C compiler by default, so MinGW-w64 fills that gap.


Install it with:
```
pacman -S --needed base-devel mingw-w64-x86_64-toolchain
```
![img](/docs/images/compile.png)

Once installed, **close and reopen** the MSYS2 terminal again (same as before).

### Step 6: Install MinGW-w64 Go Package

Install the Go toolchain for building native Windows 64-bit binaries:
```
pacman -S mingw-w64-x86_64-go
```
After installation, close the MSYS2 terminal.

### Step 7: Set Envrionment Variable
To enable access to the installed tools from any terminal or build script:

1. Open **System Environment Variables** (search "Edit environment variables" in the Start menu).
2. Append the following path to your system `Path` variable:
	```
	C:\msys64\mingw64\bin
	```
3. Add the following environment variables and append them to your your system `Path` variable:

	- `GOPATH = C:\msys64\mingw64\go`
	- `GOROOT = <Go installation path>` (e.g., `C:\Program Files\Go`)

### Step 9: Verify Installation

Open a Command Prompt and run:
```
gcc --version
```
You should see the installed GCC version:
![img](/docs/images/gcc-version.png)

### Step 9: Build yc-360 Script
Once everything is set up, navigate to the yc-360 script source directory  `yc-data-script/cmd/yc` and run:
```
go build
```
This will generate a final executable in `yc-data-script/bin/` directory.
