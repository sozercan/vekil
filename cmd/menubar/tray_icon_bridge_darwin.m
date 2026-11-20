//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <objc/runtime.h>
#import "tray_icon_bridge.h"

@interface VekilRunningBadgeView : NSView
@end

@implementation VekilRunningBadgeView

- (NSView *)hitTest:(NSPoint)point {
    return nil;
}

- (BOOL)isAccessibilityElement {
    return NO;
}

- (BOOL)allowsVibrancy {
    return NO;
}

- (void)drawRect:(NSRect)dirtyRect {
    NSButton *button = (NSButton *)self.superview;
    NSRect imageRect = [button.cell imageRectForBounds:button.bounds];
    NSPoint center = NSMakePoint(NSMinX(imageRect) + VEKIL_TRAY_BADGE_CENTER_X,
                                NSMinY(imageRect) + VEKIL_TRAY_BADGE_CENTER_Y);
    if (button.isFlipped) {
        center.y = NSMaxY(imageRect) - VEKIL_TRAY_BADGE_CENTER_Y;
    }
    center = [self convertPoint:center fromView:button];
    CGFloat radius = VEKIL_TRAY_BADGE_RADIUS;
    NSRect circle = NSMakeRect(center.x - radius, center.y - radius, radius * 2, radius * 2);
    // a darker green gives the small white check more contrast on light bars.
    [[NSColor colorWithSRGBRed:0 green:136.0/255.0 blue:0 alpha:1] setFill];
    [[NSBezierPath bezierPathWithOvalInRect:circle] fill];

    NSBezierPath *check = [NSBezierPath bezierPath];
    [check moveToPoint:NSMakePoint(center.x - 2.3, center.y)];
    [check lineToPoint:NSMakePoint(center.x - 0.6, center.y - 1.5)];
    [check lineToPoint:NSMakePoint(center.x + 2.4, center.y + 1.8)];
    check.lineWidth = 1.4;
    check.lineCapStyle = NSLineCapStyleRound;
    check.lineJoinStyle = NSLineJoinStyleRound;
    [NSColor.whiteColor setStroke];
    [check stroke];
}

@end

// fyne.io/systray v1.12.2 owns this private ivar and provides no public badge
// API. Keep the compatibility check here, with a template-only fallback in Go.
// This uses the Objective-C runtime, not private AppKit APIs or method swizzling.
static NSStatusBarButton *vekil_tray_button(id delegate) {
    Class ownerClass = NSClassFromString(@"SystrayAppDelegate");
    if (ownerClass == Nil || ![delegate isKindOfClass:ownerClass]) {
        return nil;
    }
    Ivar variable = class_getInstanceVariable(ownerClass, "statusItem");
    if (variable == NULL || ivar_getTypeEncoding(variable)[0] != '@') {
        return nil;
    }
    id item = object_getIvar(delegate, variable);
    if (![item isKindOfClass:NSStatusItem.class]) {
        return nil;
    }
    NSStatusBarButton *button = [item button];
    return [button isKindOfClass:NSStatusBarButton.class] ? button : nil;
}

static void vekil_tray_apply_icon(NSStatusBarButton *button, NSImage *image, BOOL running) {
    image.size = NSMakeSize(VEKIL_TRAY_IMAGE_POINTS, VEKIL_TRAY_IMAGE_POINTS);
    image.template = YES;
    button.image = image;
    button.imagePosition = button.title.length == 0 ? NSImageOnly : NSImageLeft;

    VekilRunningBadgeView *badge = nil;
    for (NSView *view in button.subviews) {
        if ([view isKindOfClass:VekilRunningBadgeView.class]) {
            badge = (VekilRunningBadgeView *)view;
            break;
        }
    }
    if (badge == nil && running) {
        badge = [[VekilRunningBadgeView alloc] initWithFrame:button.bounds];
        badge.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
        [button addSubview:badge];
    }
    badge.hidden = !running;
    badge.needsDisplay = YES;
}

const char *vekil_tray_set_icon(const void *bytes, int length, bool running) {
    @autoreleasepool {
        NSImage *image = [[NSImage alloc] initWithData:[NSData dataWithBytes:bytes length:length]];
        if (image == nil) {
            return "could not decode robot template";
        }
        __block const char *errorMessage = NULL;
        dispatch_block_t update = ^{
            NSStatusBarButton *button = vekil_tray_button(NSApp.delegate);
            if (button == nil) {
                errorMessage = "systray status button unavailable";
                return;
            }
            vekil_tray_apply_icon(button, image, running);
        };
        if (NSThread.isMainThread) {
            update();
        } else {
            dispatch_sync(dispatch_get_main_queue(), update);
        }
        return errorMessage;
    }
}
