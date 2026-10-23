#include <stdio.h>

#include "messages.h"

int main(void) {
    for (int i = 0; i < MESSAGE_COUNT; i++)
        puts(MESSAGES[i]);
    return 0;
}
