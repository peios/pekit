#include <stdio.h>

#include "greet.h"
#include "internal.h"

const char *greeting(void) {
    return GREETING;
}

void greet(const char *name) {
    printf("%s, %s\n", greeting(), name);
}
