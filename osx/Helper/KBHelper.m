//
//  KBHelper.m
//  Keybase
//
//  Created by Gabriel on 4/20/15.
//  Copyright (c) 2015 Gabriel Handford. All rights reserved.
//

#import "KBHelper.h"

#include <syslog.h>
#include <xpc/xpc.h>
#import <MPMessagePack/MPMessagePack.h>
#import "KBFSInstaller.h"
#import "KBHelperDefines.h"

@interface KBHelper () <NSXPCListenerDelegate>
@property xpc_connection_t connection;

@property KBFSInstaller *kbfsInstaller;
@end

@implementation KBHelper

- (void)listen:(xpc_connection_t)service {
  xpc_connection_set_event_handler(service, ^(xpc_object_t connection) {

    [self log:@"Setting connection event handler."];
    xpc_connection_set_event_handler(connection, ^(xpc_object_t event) {

      [self log:[NSString stringWithFormat:@"Received event: %@", event]];

      xpc_type_t type = xpc_get_type(event);

      if (type == XPC_TYPE_ERROR) {
        if (event == XPC_ERROR_CONNECTION_INVALID) {
          // The client process on the other end of the connection has either
          // crashed or cancelled the connection. After receiving this error,
          // the connection is in an invalid state, and you do not need to
          // call xpc_connection_cancel(). Just tear down any associated state
          // here.
        } else if (event == XPC_ERROR_TERMINATION_IMMINENT) {
          // Handle per-connection termination cleanup.
        }
      } else {
        xpc_connection_t remote = xpc_dictionary_get_remote_connection(event);
        [self handleEvent:event completion:^(NSError* error, NSData *data) {
          if (error) {
            xpc_object_t reply = xpc_dictionary_create_reply(event);
            xpc_dictionary_set_string(reply, "error", [[error localizedDescription] UTF8String]);
            xpc_connection_send_message(remote, reply);
          } else {
            xpc_object_t reply = xpc_dictionary_create_reply(event);
            xpc_dictionary_set_data(reply, "data", [data bytes], [data length]);
            xpc_connection_send_message(remote, reply);
          }
        }];
      }
    });

    xpc_connection_resume(connection);
  });

  xpc_connection_resume(service);
}

- (void)handleEvent:(xpc_object_t)event completion:(void (^)(NSError *error, NSData *data))completion {
  size_t length = 0;
  const void *buffer = xpc_dictionary_get_data(event, "data", &length);
  NSData *dataRequest = [NSData dataWithBytes:buffer length:length];

  NSError *error = nil;

  // See msgpack-rpc spec for request/response format
  NSArray *request = [dataRequest mp_array:&error];

  if (error) {
    completion(error, nil);
  } else {
    [self handleRequest:request completion:^(NSArray *response) {
      NSData *dataResponse = [response mp_messagePack];
      completion(nil, dataResponse);
    }];
  }
}

- (void)respondWithObject:(id)obj messageId:(NSNumber *)messageId completion:(void (^)(NSArray *response))completion {
  completion(@[@(1), messageId, NSNull.null, (obj ? obj : NSNull.null)]);
}

- (void)respondWithError:(NSError *)error messageId:(id)messageId completion:(void (^)(NSArray *response))completion {
  NSDictionary *errorDict = @{@"code": @(error.code), @"desc": error.localizedDescription};
  completion(@[@(1), messageId, errorDict, NSNull.null]);
}

- (void)handleRequest:(NSArray *)request completion:(void (^)(NSArray *response))completion {
  if ([request count] != 4) {
    [self respondWithError:KBMakeError(-1, @"Invalid request (should follow msgpack-rpc spec") messageId:@(0) completion:completion];
    return;
  }

  id messageId = request[1];
  NSString *method = request[2];
  if ([method isEqualToString:@"version"]) {
    NSString *version = NSBundle.mainBundle.infoDictionary[@"CFBundleShortVersionString"];
    [self respondWithObject:version messageId:messageId completion:completion];
  } else if ([method isEqualToString:@"installKBFS"]) {
    if (!_kbfsInstaller) _kbfsInstaller = [[KBFSInstaller alloc] init];
    NSError *error = nil;
    if (![_kbfsInstaller install:&error]) {
      if (error) {
        [self respondWithError:error messageId:messageId completion:completion];
      } else {
        [self respondWithError:KBMakeError(-1000, @"Failed with unknown error") messageId:messageId completion:completion];
      }
    } else {
      [self respondWithObject:@(YES) messageId:messageId completion:completion];
    }
  } else {
    [self respondWithError:KBMakeError(-2, @"Unknown request method") messageId:messageId completion:completion];
  }
}

- (void)log:(NSString *)message {
  NSLog(@"%@", message);
  syslog(LOG_NOTICE, "%s", [message UTF8String]);
}

@end
