# JFR (Java Flight Recorder) capture

For Java applications, the yc-360 script also captures a JFR recording. It starts the recording before the other captures, lets it run while it collects them, and saves it as `my.jfr`. The recording is uploaded to the yCrash server with the other artifacts, or, with `-onlyCapture`, packed into the `yc-<timestamp>.zst` file with them.

The JVM makes the recording itself. The script sends `JFR.start` with a duration, and the JVM stops the recording and writes the file when the time is up. The script never sends `JFR.stop`, so a recording can't be left running if the script is stopped.

JFR capture works in every mode: on demand (`-p`), `-onlyCapture`, M3 and API mode. `-minimalTouch` skips it.

## Options

| Option | What it does | Default |
| --- | --- | --- |
| `jfrEnabled` | Captures a JFR recording. Set it to `false` to skip JFR. | `true` |
| `jfrCaptureDuration` | How long the recording runs, from `10s` to `5m`. | `60s` |
| `jfrSettings` | The JFR settings the recording uses: `default`, `profile`, or the path to your own `.jfc` file. | The JDK's default settings |
| `excludeSensitiveData` | Keeps the application's environment variables and system properties out of the recording. | `false` |

Pass them on the command line, or set them in the config file given with `-c`. In the config file they go under `options:`, like every other option. Keys outside `options:` are ignored without a warning.

```yaml
version: "1"
options:
  jfrSettings: /opt/jfr/my-settings.jfc
  excludeSensitiveData: true
```

## jfrSettings

`default` and `profile` are the two settings files that come with the JDK. `default` is designed to cost about 1% CPU or less. `profile` records more detail and uses more CPU, so keep `profile` recordings short.

To use your own settings, give the path to a `.jfc` file:

```sh
./yc -onlyCapture -p {PID} -j {JAVA_HOME} -jfrSettings /opt/jfr/my-settings.jfc
```

- The script reads the file itself and gives the JVM a copy. So the file only has to be readable by the user who runs the script, and it works when the JVM runs in a container.
- A relative path is relative to the directory the script runs in, also when it is set in the config file.
- The script checks the file before it starts the recording. If the file is missing, can't be read, or isn't a valid `.jfc` file, the script logs an error and records with the JDK's default settings instead, so you still get a recording. If the JVM refuses the file, the script starts the recording again with the default settings.

To make your own file, start from the JDK's `default.jfc`. It is in `$JAVA_HOME/lib/jfr/`, or in `$JAVA_HOME/jre/lib/jfr/` on JDK 8. On JDK 17 and later, `jfr configure` can also write one.

## excludeSensitiveData

By default, JFR records every environment variable and system property of the application, values included. That covers values passed as `-Dkey=value`, where passwords and tokens are often set. `excludeSensitiveData` turns off the two JFR events that record them, `jdk.InitialEnvironmentVariable` and `jdk.InitialSystemProperty`, when the recording starts:

```sh
./yc -onlyCapture -p {PID} -j {JAVA_HOME} -excludeSensitiveData
```

It overrides `jfrSettings`: the two events stay off even if your `.jfc` file turns them on.

All JFR recordings in a JVM share their data, so another recording that records these events puts them into the script's recording too. The script checks for that three times:

1. **Before the recording starts.** If another recording already records the events, for example one started with `-XX:StartFlightRecording` and the default settings, the script skips JFR. Stop that recording, or give it settings with the two events turned off.
2. **Right after the start.** The script asks the JVM to confirm that its recording has both events off and that no other recording has them on. If the JVM can't confirm it, the recording is not uploaded.
3. **After the recording is written.** The script reads the finished recording. If it holds either event, because another recording ran while the script was recording, the script deletes it and doesn't upload it.

When the script skips or deletes the recording, it logs why. The other artifacts are still captured and uploaded.

Keep in mind:

- Only those two JFR events are turned off. The JVM's command line is still recorded, in the `jdk.JVMInformation` event, and it shows any `-Dkey=value` values. The script's other artifacts, such as the process list and heap dumps, are not affected.
- When the third check deletes a recording, the values were in the JVM's temporary copy of it until the script deleted it. They were never uploaded.
- It works on JDK 8 update 262 and later, and on JDK 11 and later. On other JFR versions, such as the older JFR in Oracle JDK 7 and 8, the checks fail and no recording is uploaded.

## Checking a recording

To check that a recording holds neither event, extract it and run the JDK's `jfr` tool. The command prints nothing when both events are absent:

```sh
tar -xf yc-2024-09-23T14-02-42.zst
jfr print --events jdk.InitialEnvironmentVariable,jdk.InitialSystemProperty yc-2024-09-23T14-02-42/my.jfr
```

The `jfr` tool is in the JDK's `bin` directory. Temurin builds of JDK 8, 11, 17, 21 and 25 all include it. On Linux, `tar` needs the `zstd` package to extract the file.

## Log messages

The JFR result is in the script's log, after `JFR RECORDING DATA`. These messages explain what happened:

| Message starts with | Meaning |
| --- | --- |
| `jfrSettings: cannot use the JFR settings file` | Your `.jfc` file couldn't be used. The recording uses the JDK's default settings. |
| `jfrSettings: the JVM could not use the JFR settings` | The JVM refused the settings. The script started the recording again with the default settings. |
| `skipping JFR capture: excludeSensitiveData is on, but another JFR recording` | Check 1: another recording records the events, so there is no recording. |
| `discarding JFR recording` | Check 2 or 3 failed: the recording was deleted and not uploaded. The rest of the message says why. |
