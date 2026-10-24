#include <stdio.h>

#include "greet.h"
#include "internal.h"

void greet(const char *name) {
    printf("%s, %s\n", GREETING, name);
}
