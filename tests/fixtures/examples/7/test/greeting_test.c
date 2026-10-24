/* A unit test: links the library and checks one function directly. */

#include <stdio.h>
#include <string.h>

#include "greet.h"

int main(void) {
    if (strcmp(greeting(), "Hello") != 0) {
        fprintf(stderr, "greeting() returned \"%s\", expected \"Hello\"\n", greeting());
        return 1;
    }
    printf("ok: greeting() is \"Hello\"\n");
    return 0;
}
