#ifndef PROCPS_PROC_PWCACHE_H
#define PROCPS_PROC_PWCACHE_H

#include <sys/types.h>

// used in pwcache and in readproc to set size of username or groupname
#define P_G_SZ 33

char *pwcache_get_user(uid_t uid);
char *pwcache_get_group(gid_t gid);

#endif
