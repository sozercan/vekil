// compile the production bridge into an offscreen AppKit test executable.
#import "../tray_icon_bridge_darwin.m"

@interface VekilTestStatusItem : NSStatusItem
@property(strong) NSStatusBarButton *testButton;
@end
@implementation VekilTestStatusItem
- (NSStatusBarButton *)button { return self.testButton; }
@end

@interface SystrayAppDelegate : NSObject <NSApplicationDelegate> {
@public
    NSStatusItem *statusItem;
}
@end
@implementation SystrayAppDelegate
@end

static void require(BOOL condition, NSString *message) {
    if (!condition) {
        fprintf(stderr, "%s\n", message.UTF8String);
        exit(1);
    }
}

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        require(argc == 2, @"missing template directory");
        [NSApplication sharedApplication];
        NSString *directory = @(argv[1]);
        NSData *data = [NSData dataWithContentsOfFile:[directory stringByAppendingPathComponent:@"running.png"]];
        require(data != nil, @"missing running mask");
        require(vekil_tray_button(nil) == nil, @"nil delegate accepted");
        require(vekil_tray_button([NSObject new]) == nil, @"foreign delegate accepted");
        SystrayAppDelegate *delegate = [SystrayAppDelegate new];
        require(vekil_tray_button(delegate) == nil, @"missing status item accepted");
        VekilTestStatusItem *item = [VekilTestStatusItem new];
        item.testButton = [[NSStatusBarButton alloc] initWithFrame:NSMakeRect(0, 0, 28, 24)];
        delegate->statusItem = item;
        NSApp.delegate = delegate;
        NSStatusBarButton *button = item.button;
        require(vekil_tray_button(delegate) == button, @"could not resolve status button");

        for (int iteration = 0; iteration < 20; iteration++) {
            const char *error = vekil_tray_set_icon(data.bytes, (int)data.length, true);
            require(error == NULL, @"native icon update failed");
        }
        require(button.image.isTemplate, @"robot is not a template");
        require(NSEqualSizes(button.image.size, NSMakeSize(20, 20)), @"unexpected icon footprint");
        NSUInteger count = 0;
        VekilRunningBadgeView *badge = nil;
        for (NSView *view in button.subviews) {
            if ([view isKindOfClass:VekilRunningBadgeView.class]) {
                count++;
                badge = (VekilRunningBadgeView *)view;
            }
        }
        require(count == 1, @"badge views accumulate");
        require(!badge.hidden, @"running badge hidden");
        require([badge hitTest:NSMakePoint(14, 12)] == nil, @"badge intercepts clicks");
        require(!badge.isAccessibilityElement, @"badge adds accessibility focus");

        for (NSAppearanceName appearance in @[NSAppearanceNameAqua, NSAppearanceNameDarkAqua]) {
            button.appearance = [NSAppearance appearanceNamed:appearance];
            for (int scale = 1; scale <= 2; scale++) {
                NSBitmapImageRep *bitmap = [[NSBitmapImageRep alloc] initWithBitmapDataPlanes:NULL
                    pixelsWide:28*scale pixelsHigh:24*scale bitsPerSample:8 samplesPerPixel:4
                    hasAlpha:YES isPlanar:NO colorSpaceName:NSDeviceRGBColorSpace bytesPerRow:0 bitsPerPixel:0];
                bitmap.size = NSMakeSize(28, 24);
                [button cacheDisplayInRect:button.bounds toBitmapImageRep:bitmap];
                int greenPixels = 0;
                for (int y = 0; y < bitmap.pixelsHigh; y++) {
                    for (int x = 0; x < bitmap.pixelsWide; x++) {
                        NSColor *color = [[bitmap colorAtX:x y:y] colorUsingColorSpace:NSColorSpace.sRGBColorSpace];
                        if (color.alphaComponent > 0.5 && color.greenComponent > color.redComponent + 0.15 &&
                            color.greenComponent > color.blueComponent + 0.1) {
                            greenPixels++;
                        }
                    }
                }
                require(greenPixels > 5 * scale * scale, @"green badge missing from rendered image");
                NSString *name = [NSString stringWithFormat:@"%@-%dx.png", appearance, scale];
                NSData *png = [bitmap representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
                require([png writeToFile:[directory stringByAppendingPathComponent:name] atomically:YES], @"write render failed");
            }
        }
        for (NSString *name in @[@"plain", @"stopped"]) {
            NSData *mask = [NSData dataWithContentsOfFile:[directory stringByAppendingPathComponent:[name stringByAppendingString:@".png"]]];
            require(vekil_tray_set_icon(mask.bytes, (int)mask.length, false) == NULL, @"stop icon failed");
            require(badge.hidden, @"non-running icon retains green badge");
        }
        NSApp.delegate = nil;
        require(vekil_tray_set_icon(data.bytes, (int)data.length, true) != NULL, @"adapter failure not reported");
    }
    return 0;
}
