#include <stdio.h>
#include <stdlib.h>

int getenv_int(const char *name) {
	char *val, *endptr;
	int ret;

	val = getenv(name);
	if (val == NULL || *val == '\0')
		return -1;

	ret = strtol(val, &endptr, 10);
	return ret;
}