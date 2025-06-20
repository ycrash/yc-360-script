# Building the yc-360 Script on macOS

This guide outlines the steps to build the yc-360 script in macOS environments.

---
Ensure the following packages are installed.

**Required Packages:**
- go
- git

## Build yc-360 Script

### Step 1:  Navigate to the `cmd/yc` directory inside the repository:
```
cd ../yc-data-script/cmd/yc
```
### Step 2: Then run the following command to build the yc-360 script:
```
go build -o yc -ldflags='-s -w' -buildvcs=false
```
Once the build is completed, the yc binary will be available in the `../yc-data-script/bin/` directory.
