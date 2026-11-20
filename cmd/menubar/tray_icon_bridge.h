#include <stdbool.h>

// shared geometry keeps the raster mask and native vector badge aligned.
#define VEKIL_TRAY_IMAGE_POINTS 20
#define VEKIL_TRAY_BADGE_CENTER_X 15
#define VEKIL_TRAY_BADGE_CENTER_Y 5
#define VEKIL_TRAY_BADGE_RADIUS 4.5
#define VEKIL_TRAY_BADGE_CLEARANCE_RADIUS 5.5

// returns a static error message, or NULL on success. No Go memory is retained.
const char *vekil_tray_set_icon(const void *bytes, int length, bool running);
