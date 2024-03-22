/*
 * File for parsing top-level /proc entities.
 * Copyright (C) 1992-1998 by Michael K. Johnson, johnsonm@redhat.com
 * Copyright 1998-2003 Albert Cahalan
 * June 2003, Fabian Frederick, slab info
 *
 * This library is free software; you can redistribute it and/or
 * modify it under the terms of the GNU Lesser General Public
 * License as published by the Free Software Foundation; either
 * version 2.1 of the License, or (at your option) any later version.
 *
 * This library is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the GNU
 * Lesser General Public License for more details.
 *
 * You should have received a copy of the GNU Lesser General Public
 * License along with this library; if not, write to the Free Software
 * Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA  02110-1301  USA
 */

#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <ctype.h>
#include <locale.h>
#include <errno.h>

#include <unistd.h>
#include <fcntl.h>
#ifdef __CYGWIN__
#include <sys/param.h>
#endif
#include "misc.h"
#include "procps-private.h"


#define BAD_OPEN_MESSAGE					\
"Error: /proc must be mounted\n"				\
"  To mount /proc at boot you need an /etc/fstab line like:\n"	\
"      proc   /proc   proc    defaults\n"			\
"  In the meantime, run \"mount proc /proc -t proc\"\n"

#define LOADAVG_FILE "/proc/loadavg"
static int loadavg_fd = -1;

// As of 2.6.24 /proc/meminfo seems to need 888 on 64-bit,
// and would need 1258 if the obsolete fields were there.
// As of 3.13 /proc/vmstat needs 2623,
// and /proc/stat needs 3076.
static __thread char buf[8192];

/* This macro opens filename only if necessary and seeks to 0 so
 * that successive calls to the functions are more efficient.
 * It also reads the current contents of the file into the global buf.
 */
#define FILE_TO_BUF(filename, fd) do{				\
    static __thread int local_n;				\
    if (fd == -1 && (fd = open(filename, O_RDONLY)) == -1) {	\
	fputs(BAD_OPEN_MESSAGE, stderr);			\
	fflush(NULL);						\
	_exit(102);						\
    }								\
    lseek(fd, 0L, SEEK_SET);					\
    if ((local_n = read(fd, buf, sizeof buf - 1)) < 0) {	\
	perror(filename);					\
	fflush(NULL);						\
	_exit(103);						\
    }								\
    buf[local_n] = '\0';					\
}while(0)

/* evals 'x' twice */
#define SET_IF_DESIRED(x,y) do{  if(x) *(x) = (y); }while(0)

/* return minimum of two values */
#ifndef __CYGWIN__
#define MIN(x,y) ((x) < (y) ? (x) : (y))
#endif

/*
 * procps_hertz_get:
 *
 *
 * Some values in /proc are expressed in units of 1/HZ seconds, where HZ
 * is the kernel clock tick rate. One of these units is called a jiffy.
 * The HZ value used in the kernel may vary according to hacker desire.
 *
 * On some architectures, the kernel provides an ELF note to indicate
 * HZ.
 *
 * Returns:
 *  The discovered or assumed hertz value
 */
PROCPS_EXPORT long procps_hertz_get(void)
{
    long hz;

#ifdef _SC_CLK_TCK
    if ((hz = sysconf(_SC_CLK_TCK)) > 0)
        return hz;
#endif
#ifdef HZ
    return(HZ);
#endif
    /* Last resort, assume 100 */
    return 100;
}

/*
 * procps_loadavg:
 * @av1: location to store 1 minute load average
 * @av5: location to store 5 minute load average
 * @av15: location to store 15 minute load average
 *
 * Find the 1,5 and 15 minute load average of the system
 *
 * Returns: 0 on success <0 on error
 */
PROCPS_EXPORT int procps_loadavg(
        double *restrict av1,
        double *restrict av5,
        double *restrict av15)
{
    double avg_1=0, avg_5=0, avg_15=0;
    locale_t tmplocale;
    int retval=0;

    FILE_TO_BUF(LOADAVG_FILE,loadavg_fd);
    tmplocale = newlocale(LC_NUMERIC_MASK, "C", (locale_t)0);
    uselocale(tmplocale);
    if (sscanf(buf, "%lf %lf %lf", &avg_1, &avg_5, &avg_15) < 3)
        retval = -ERANGE;

    uselocale(LC_GLOBAL_LOCALE);
    freelocale(tmplocale);
    SET_IF_DESIRED(av1,  avg_1);
    SET_IF_DESIRED(av5,  avg_5);
    SET_IF_DESIRED(av15, avg_15);
    return retval;
}

/////////////////////////////////////////////////////////////////////////////

#define PROCFS_PID_MAX "/proc/sys/kernel/pid_max"
#define DEFAULT_PID_LENGTH 5

/*
 * procps_pid_length
 *
 * Return the length of the maximum possible pid.
 *
 * Returns either the strlen of PROCFS_PID_MAX or the
 * best-guess DEFAULT_PID_LENGTH
 */
PROCPS_EXPORT unsigned int procps_pid_length(void)
{
    FILE *fp;
    char pidbuf[24];
    static __thread int pid_length=0;

    if (pid_length)
        return pid_length;

    pid_length = DEFAULT_PID_LENGTH;
    if ((fp = fopen(PROCFS_PID_MAX, "r")) != NULL) {
        if (fgets(pidbuf, sizeof(pidbuf), fp) != NULL) {
            pid_length = strlen(pidbuf);
            if (pidbuf[pid_length-1] == '\n')
                --pid_length;
        }
        fclose(fp);
    }
    return pid_length;
}

///////////////////////////////////////////////////////////////////////////

/* procps_cpu_count:
 *
 * Returns the number of CPUs that are currently online.
 *
 */
long procps_cpu_count(void)
{
    long cpus;

    cpus = sysconf(_SC_NPROCESSORS_ONLN);
    if (cpus < 1)
        return 1;
    return cpus;
}

