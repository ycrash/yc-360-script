# Building the yCrash Agent on macOS

This guide outlines the steps to build the yCrash agent in macOS environments.

---
Ensure the following packages are installed.

**Required Packages:**
- go
- git

## Build yCrash Agent

### Step 1:  Navigate to the `cmd/yc` directory inside the repository:
```
cd ../yc-data-script/cmd/yc
```
### Step 2: Then run the following command to build the agent:
```
go build -o yc -ldflags='-s -w' -buildvcs=false
```
Once the build is completed, the yc binary will be available in the `../yc-data-script/bin/` directory.
