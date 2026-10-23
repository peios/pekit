/* embed: turn a file into C source, so a program can carry the file inside it.
 *
 * Usage: embed INPUT NAME OUTDIR
 *
 * Writes OUTDIR/NAME.h, declaring `extern const char NAME[];`, and
 * OUTDIR/NAME.c, defining NAME as the bytes of INPUT followed by a NUL. */

#include <stdio.h>
#include <stdlib.h>

static FILE *open_out(const char *outdir, const char *name, const char *ext) {
    char path[4096];
    snprintf(path, sizeof path, "%s/%s.%s", outdir, name, ext);
    FILE *f = fopen(path, "w");
    if (!f) {
        perror(path);
        exit(1);
    }
    return f;
}

int main(int argc, char **argv) {
    if (argc != 4) {
        fprintf(stderr, "usage: embed INPUT NAME OUTDIR\n");
        return 2;
    }
    const char *input = argv[1], *name = argv[2], *outdir = argv[3];

    FILE *in = fopen(input, "rb");
    if (!in) {
        perror(input);
        return 1;
    }

    FILE *h = open_out(outdir, name, "h");
    fprintf(h, "extern const char %s[];\n", name);
    fclose(h);

    FILE *c = open_out(outdir, name, "c");
    fprintf(c, "const char %s[] = {", name);
    int ch;
    while ((ch = fgetc(in)) != EOF)
        fprintf(c, "%d,", ch);
    fprintf(c, "0};\n");
    fclose(c);

    fclose(in);
    return 0;
}
