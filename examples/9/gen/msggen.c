/* msggen: turn a list of messages into C, one source file per message.
 *
 * Usage: msggen INPUT OUTDIR
 *
 * Each line of INPUT is "NAME TEXT". For each line, msggen writes
 * OUTDIR/msg_NAME.c defining `const char msg_NAME[]`. It also writes
 * OUTDIR/messages.h, declaring the table, and OUTDIR/table.c, defining
 * MESSAGES and MESSAGE_COUNT. How many files it writes depends on INPUT, so
 * nobody knows them until msggen has run. */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define MAX_MESSAGES 256

static FILE *open_out(const char *outdir, const char *name) {
    char path[4096];
    snprintf(path, sizeof path, "%s/%s", outdir, name);
    FILE *f = fopen(path, "w");
    if (!f) {
        perror(path);
        exit(1);
    }
    return f;
}

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: msggen INPUT OUTDIR\n");
        return 2;
    }
    FILE *in = fopen(argv[1], "r");
    if (!in) {
        perror(argv[1]);
        return 1;
    }

    static char names[MAX_MESSAGES][64];
    int count = 0;
    char line[1024];
    while (fgets(line, sizeof line, in)) {
        line[strcspn(line, "\n")] = '\0';
        char *space = strchr(line, ' ');
        if (!space || count == MAX_MESSAGES)
            continue;
        *space = '\0';
        snprintf(names[count], sizeof names[count], "%.60s", line);

        char file[128];
        snprintf(file, sizeof file, "msg_%.60s.c", names[count]);
        FILE *c = open_out(argv[2], file);
        fprintf(c, "const char msg_%s[] = \"%s\";\n", names[count], space + 1);
        fclose(c);
        count++;
    }
    fclose(in);

    FILE *h = open_out(argv[2], "messages.h");
    fprintf(h, "extern const char *const MESSAGES[];\nextern const int MESSAGE_COUNT;\n");
    fclose(h);

    FILE *t = open_out(argv[2], "table.c");
    for (int i = 0; i < count; i++)
        fprintf(t, "extern const char msg_%s[];\n", names[i]);
    fprintf(t, "const char *const MESSAGES[] = {");
    for (int i = 0; i < count; i++)
        fprintf(t, "%smsg_%s", i ? ", " : "", names[i]);
    fprintf(t, "};\nconst int MESSAGE_COUNT = %d;\n", count);
    fclose(t);
    return 0;
}
