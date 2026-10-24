#include <stdio.h>
#include <string.h>

#include <zlib.h>

int main(void) {
    const char *text = "pekit2 pekit2 pekit2 pekit2 pekit2 pekit2 pekit2 pekit2";
    unsigned char packed[256];
    char unpacked[256];
    uLongf packed_len = sizeof packed;
    uLongf unpacked_len = sizeof unpacked;

    if (compress(packed, &packed_len, (const Bytef *)text, strlen(text) + 1) != Z_OK)
        return 1;
    if (uncompress((Bytef *)unpacked, &unpacked_len, packed, packed_len) != Z_OK)
        return 1;

    printf("zlib %s: %zu bytes -> %lu bytes -> \"%s\"\n",
           zlibVersion(), strlen(text) + 1, (unsigned long)packed_len, unpacked);
    return 0;
}
