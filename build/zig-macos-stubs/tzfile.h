/* tzfile.h stub for zig macOS cross-compile.
 * ICU 75.1 putil.cpp includes tzfile.h unconditionally under __APPLE__;
 * zig's bundled macOS SDK omits it (removed from modern Apple SDKs).
 * ICU only uses TZDIR and TZDEFAULT from this header. */
#ifndef TZFILE_H
#define TZFILE_H
#ifndef TZDIR
#define TZDIR "/var/db/timezone/zoneinfo"
#endif
#ifndef TZDEFAULT
#define TZDEFAULT "/etc/localtime"
#endif
#endif
