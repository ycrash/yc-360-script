import java.io.File;
import java.util.ArrayList;
import java.util.List;

/**
 * Reproduces a JVM whose heap is so full that JFR.start fails, while the JVM itself
 * stays alive and attachable.
 *
 * Usage: java -Xmx32m -XX:+UseSerialGC -cp . FillHeapAndHold <ready-file> [reserve-bytes]
 *
 * Steps:
 *   1. Set aside a reserve allocation (2 MB unless reserve-bytes is given).
 *   2. Fill the rest of the heap with byte arrays, using smaller and smaller chunks
 *      until even a 16-byte allocation fails.
 *   3. Release the reserve. That leaves enough headroom for this thread to keep
 *      running, but not enough for JFR to start a recording.
 *   4. Create the ready file and print a line to stdout, so the caller knows the
 *      heap is full.
 *   5. Sleep forever.
 *
 * Why not use BuggyApp (github.com/ycrash/buggyapp)? None of its heap scenarios
 * leaves the JVM both alive and with a full heap:
 *   - PROBLEM_OOM (bug1): catches the OutOfMemoryError, but the map that filled the
 *     heap is only referenced from a local variable, so the next GC frees it all.
 *     The JVM then exits after 5 seconds.
 *   - bug1.1: nothing catches the OutOfMemoryError, so main dies and the JVM exits.
 *   - PROBLEM_MEMORY (bug1.2): stays alive, but stops growing as soon as free heap
 *     drops below 50 MB. The tens of MB still free are plenty for JFR.start.
 *   - oomcrash.OOMNoCrash / OOMCrash / LimitlessArray: the heap is freed or the JVM
 *     exits after the OutOfMemoryError.
 * This program differs in two ways: everything stays reachable through a static
 * field, so the GC cannot reclaim it, and it fills the heap to the last 16 bytes
 * and then frees only a known reserve. That makes the JFR.start failure deterministic.
 *
 * How the reserve size changes JFR.start, measured on Temurin 8u504-b01 with
 * -Xmx32m -XX:+UseSerialGC (fresh JVM each time, same result on repeat runs):
 *   64-128 KB:  JFR.start prints a lambda-form dump; JFR.check lists nothing
 *   256-512 KB: "Java heap space"; JFR.check lists nothing
 *   2 MB:       "Java heap space"; JFR.check lists the recording as (new). This is
 *               the customer's scenario, so it is the default.
 *   4 MB+:      the recording starts normally
 * Other JDK builds, heap sizes or collectors may need a different reserve.
 */
public class FillHeapAndHold {
  private static final int DEFAULT_RESERVE_BYTES = 2 * 1024 * 1024;
  private static final int FIRST_CHUNK_BYTES = 1024 * 1024;
  private static final int SMALLEST_CHUNK_BYTES = 16;

  // Static fields so the allocations stay reachable and the GC cannot free them.
  // The list is presized so that adding to it does not need a new backing array
  // while the heap is nearly full.
  private static final List<Object> hold = new ArrayList<>(1024 * 1024);
  private static byte[] reserve;

  public static void main(String[] args) throws Exception {
    String readyFile = args[0];
    int reserveBytes = args.length > 1 ? Integer.parseInt(args[1]) : DEFAULT_RESERVE_BYTES;

    reserve = new byte[reserveBytes];
    fillHeap();
    reserve = null;

    signalReady(readyFile);
    sleepForever();
  }

  /** Allocates chunks until the heap is exhausted, halving the chunk size on each failure. */
  private static void fillHeap() {
    int chunkSize = FIRST_CHUNK_BYTES;
    while (true) {
      try {
        hold.add(new byte[chunkSize]);
      } catch (Throwable outOfMemory) {
        if (chunkSize <= SMALLEST_CHUNK_BYTES) {
          return;
        }
        chunkSize /= 2;
      }
    }
  }

  // Both signals are best effort and independent: with the heap this full, either
  // can fail, and one failing should not stop the other.
  private static void signalReady(String path) {
    try {
      new File(path).createNewFile();
    } catch (Throwable ignored) {
    }
    try {
      // System.out auto-flushes on println, so the line appears straight away.
      System.out.println("FillHeapAndHold: heap is full, ready (" + path + ")");
    } catch (Throwable ignored) {
    }
  }

  private static void sleepForever() {
    while (true) {
      try {
        Thread.sleep(1000);
      } catch (Throwable ignored) {
        // Keep sleeping, even after an OutOfMemoryError or an interrupt.
      }
    }
  }
}
